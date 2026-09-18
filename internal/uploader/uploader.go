// Package uploader drains the retry-stable spool over signed HTTPS.
package uploader

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Taurine-Technology/axon-contracts/gen/go/buildinfo"
	"github.com/Taurine-Technology/axon-contracts/gen/go/compress"
	"github.com/Taurine-Technology/axon-pulse/internal/protocol"
	"github.com/Taurine-Technology/axon-pulse/internal/spool"
	"github.com/Taurine-Technology/axon-pulse/internal/state"
)

type (
	Snapshot struct {
		LastSuccess   time.Time `json:"last_success,omitzero"`
		LastAttempt   time.Time `json:"last_attempt,omitzero"`
		NextAttempt   time.Time `json:"next_attempt,omitzero"`
		LastError     string    `json:"last_error,omitempty"`
		Failures      int       `json:"failures"`
		UploadedBytes int64     `json:"uploaded_bytes"`
	}

	Uploader struct {
		store  *state.Store
		spool  *spool.Spool
		client *protocol.Client
		log    *slog.Logger
		start  time.Time
		now    func() time.Time

		mu             sync.RWMutex
		runMu          sync.Mutex
		snapshot       Snapshot
		lastHeartbeat  time.Time
		force          bool
		speedRequest   *protocol.SpeedTestRequest
		privacyBlocked bool
	}
)

const (
	minimumBackoff = 2 * time.Second
	maximumBackoff = 5 * time.Minute
	// quotaBackoff paces retries after an HTTP 429 that carries no
	// Retry-After. Quotas reset on quota-scale horizons; polling one every
	// few minutes only burns power and log space on both ends.
	quotaBackoff = 30 * time.Minute
	// maximumRetryAfter caps how long a server-supplied Retry-After may push
	// the sensor out, so a misconfigured controller cannot silence a sensor
	// indefinitely.
	maximumRetryAfter = 6 * time.Hour
)

func New(store *state.Store, queue *spool.Spool, client *protocol.Client, logger *slog.Logger) *Uploader {
	if client == nil {
		client = protocol.NewClient()
	}
	if logger == nil {
		logger = slog.Default()
	}
	now := time.Now()
	return &Uploader{store: store, spool: queue, client: client, log: logger, start: now, now: time.Now, snapshot: Snapshot{NextAttempt: now}}
}

func (u *Uploader) Force() {
	u.mu.Lock()
	u.force = true
	u.snapshot.NextAttempt = time.Time{}
	u.mu.Unlock()
}

func (u *Uploader) Snapshot() Snapshot {
	u.mu.RLock()
	defer u.mu.RUnlock()
	return u.snapshot
}

func (u *Uploader) PendingSpeedTestRequest() *protocol.SpeedTestRequest {
	u.mu.RLock()
	defer u.mu.RUnlock()
	if u.speedRequest == nil {
		return nil
	}
	request := *u.speedRequest
	return &request
}

func (u *Uploader) ClearSpeedTestRequest(nonce string) {
	u.mu.Lock()
	if u.speedRequest != nil && u.speedRequest.Nonce == nonce {
		u.speedRequest = nil
	}
	u.mu.Unlock()
}

// ClearSpeedTestRequests drops any controller directive when the trust or
// operating-mode boundary changes. A request authenticated under a previous
// enrollment must never survive a disconnect, revocation, or local-only mode.
func (u *Uploader) ClearSpeedTestRequests() {
	u.mu.Lock()
	u.speedRequest = nil
	u.mu.Unlock()
}

// ClearPrivacyBlock releases the in-memory revocation latch only after the
// service has completed the corresponding durable cleanup under its mode
// writer lock.
func (u *Uploader) ClearPrivacyBlock() {
	u.mu.Lock()
	u.privacyBlocked = false
	u.mu.Unlock()
}

// Tick performs at most one network request. The daemon calls it from its
// one-second scheduler; retries therefore never create an unbounded goroutine.
func (u *Uploader) Tick(ctx context.Context) error {
	u.runMu.Lock()
	defer u.runMu.Unlock()
	now := u.now()
	u.mu.Lock()
	if !u.force && now.Before(u.snapshot.NextAttempt) {
		u.mu.Unlock()
		return nil
	}
	u.force = false
	u.snapshot.LastAttempt = now
	u.mu.Unlock()

	current := u.store.Snapshot()
	u.mu.RLock()
	privacyBlocked := u.privacyBlocked
	u.mu.RUnlock()
	if privacyBlocked {
		// Retry the durable marker even when the Store is already blocked in
		// memory: the prior attempt may have failed both state and sentinel I/O.
		if err := u.store.BeginPrivacyPurge(state.PrivacyPurgeRevocation); err != nil {
			u.fail(err, 0)
			return fmt.Errorf("persist revoked-sensor privacy block: %w", err)
		}
		u.scheduleNormal(current.Config.UploadIntervalSeconds)
		return protocol.ErrRevoked
	}
	if current.SSIDPurgePending {
		u.scheduleNormal(current.Config.UploadIntervalSeconds)
		return nil
	}
	if !current.Claimed() || current.Mode != state.ModeConnected {
		u.scheduleNormal(current.Config.UploadIntervalSeconds)
		return nil
	}
	credentials := protocol.Credentials{SensorID: current.SensorID, Secret: current.SensorSecret}
	pending, err := u.spool.NextBatch(ctx, appVersion(), current.ConfigVersion)
	if err != nil {
		u.fail(err, 0)
		return err
	}
	if pending != nil {
		if err := protocol.ValidateIngestURL(current.ControllerURL, current.IngestURL); err != nil {
			u.fail(err, 0)
			return err
		}
		wireBody := compress.Encode(pending.Body)
		response, err := u.client.Ingest(ctx, current.IngestURL, credentials, wireBody)
		if err != nil {
			if errors.Is(err, protocol.ErrRevoked) {
				return u.revoke(err)
			}
			u.fail(err, retryAfter(err))
			return err
		}
		if err := u.spool.Ack(ctx, pending.Batch.BatchID); err != nil {
			u.fail(err, 0)
			return err
		}
		u.succeed(int64(len(wireBody)))
		if response.RejectedRecords > 0 {
			// The controller accepts a batch and silently drops records
			// that fail its strict validation; without this line a
			// contract drift loses evidence with no trace on either side.
			u.log.Warn("sensor_upload_records_rejected", "batch_id", pending.Batch.BatchID, "rejected", response.RejectedRecords, "records", len(pending.RecordIDs))
		}
		u.queueSpeedTestRequest(response.SpeedTestRequest, credentials)
		if response.ConfigVersion > current.ConfigVersion {
			if err := u.pullConfig(ctx, current.ControllerURL, credentials); err != nil {
				if errors.Is(err, protocol.ErrRevoked) {
					return u.revoke(err)
				}
				u.log.Warn("sensor_config_pull_failed", "error", err)
			}
		}
		stats, statsErr := u.spool.Stats(ctx)
		if statsErr == nil && stats.Records > 0 {
			u.schedule(time.Second)
		} else {
			u.scheduleNormal(current.Config.UploadIntervalSeconds)
		}
		return nil
	}

	u.mu.RLock()
	heartbeatDue := u.lastHeartbeat.IsZero() || now.Sub(u.lastHeartbeat) >= time.Duration(current.Config.HeartbeatIntervalSeconds)*time.Second
	u.mu.RUnlock()
	if heartbeatDue {
		stats, err := u.spool.Stats(ctx)
		if err != nil {
			u.fail(err, 0)
			return err
		}
		response, err := u.client.Heartbeat(ctx, current.ControllerURL, credentials, protocol.HeartbeatRequest{
			AppVersion: appVersion(), UptimeSeconds: int64(now.Sub(u.start).Seconds()),
			SpoolRecords: stats.Records, SpoolBytes: stats.Bytes,
		})
		if err != nil {
			if errors.Is(err, protocol.ErrRevoked) {
				return u.revoke(err)
			}
			u.fail(err, retryAfter(err))
			return err
		}
		u.mu.Lock()
		u.lastHeartbeat = now
		u.mu.Unlock()
		u.queueSpeedTestRequest(response.SpeedTestRequest, credentials)
		u.succeed(0)
		if response.ConfigVersion > current.ConfigVersion {
			if err := u.pullConfig(ctx, current.ControllerURL, credentials); err != nil {
				if errors.Is(err, protocol.ErrRevoked) {
					return u.revoke(err)
				}
				u.log.Warn("sensor_config_pull_failed", "error", err)
			}
		}
	}
	u.scheduleNormal(current.Config.UploadIntervalSeconds)
	return nil
}

func (u *Uploader) queueSpeedTestRequest(request *protocol.SpeedTestRequest, credentials protocol.Credentials) {
	if request == nil {
		return
	}
	if err := protocol.VerifySpeedTestRequest(*request, credentials); err != nil {
		u.log.Warn("controller_speed_test_request_rejected", "reason", "invalid_signature")
		return
	}
	pending := *request
	u.mu.Lock()
	u.speedRequest = &pending
	u.mu.Unlock()
}

func (u *Uploader) pullConfig(ctx context.Context, controllerURL string, credentials protocol.Credentials) error {
	response, err := u.client.Config(ctx, controllerURL, credentials)
	if err != nil {
		return err
	}
	if err := u.store.SetConfig(response.Config, response.ConfigVersion); err != nil {
		return fmt.Errorf("persist sensor config: %w", err)
	}
	u.log.Info("sensor_config_updated", "config_version", response.ConfigVersion)
	return nil
}

func (u *Uploader) revoke(cause error) error {
	u.mu.Lock()
	// Set the volatile latch before attempting the durable marker. A state
	// write failure must never permit the same process to retransmit.
	u.privacyBlocked = true
	u.speedRequest = nil
	u.snapshot.LastError = protocol.ErrRevoked.Error()
	u.snapshot.Failures = 0
	u.snapshot.NextAttempt = time.Time{}
	u.mu.Unlock()
	if err := u.store.BeginPrivacyPurge(state.PrivacyPurgeRevocation); err != nil {
		u.fail(err, 0)
		return fmt.Errorf("persist revoked-sensor privacy block: %w", err)
	}
	u.scheduleNormal(u.store.Snapshot().Config.UploadIntervalSeconds)
	u.log.Warn("sensor_revoked", "error", cause)
	return protocol.ErrRevoked
}

func (u *Uploader) succeed(bytesSent int64) {
	u.mu.Lock()
	u.snapshot.LastSuccess = u.now()
	u.snapshot.LastError = ""
	u.snapshot.Failures = 0
	u.snapshot.UploadedBytes += bytesSent
	u.mu.Unlock()
}

func (u *Uploader) fail(err error, retry time.Duration) {
	// An explicit server Retry-After is honored up to its own cap; the
	// sensor's short exponential ceiling applies only to its own guesses.
	ceilingCap := maximumBackoff
	if retry > 0 {
		ceilingCap = maximumRetryAfter
	}
	u.mu.Lock()
	u.snapshot.LastError = err.Error()
	u.snapshot.Failures++
	if retry <= 0 {
		exponent := min(u.snapshot.Failures-1, 8)
		ceiling := min(minimumBackoff*time.Duration(1<<exponent), maximumBackoff)
		retry = fullJitter(ceiling)
		var apiErr *protocol.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusTooManyRequests {
			// A quota rejection will not heal on the five-minute horizon.
			base := quotaBackoff
			if strings.Contains(apiErr.Code, "quota") {
				// Daily/monthly quota windows reset on hours-scale horizons;
				// escalate consecutive rejections so a sensor makes a handful
				// of attempts per window, not one every half hour.
				base = min(quotaBackoff*time.Duration(1<<min(u.snapshot.Failures-1, 4)), maximumRetryAfter)
			}
			retry = base + fullJitter(base)
			ceilingCap = maximumRetryAfter
		}
	}
	if retry > ceilingCap {
		retry = ceilingCap
	}
	u.snapshot.NextAttempt = u.now().Add(retry)
	u.mu.Unlock()
	u.log.Warn("sensor_upload_failed", "error", err, "retry_in", retry)
}

func (u *Uploader) scheduleNormal(seconds int) {
	if seconds <= 0 {
		seconds = 60
	}
	base := time.Duration(seconds) * time.Second
	spread := base / 10
	delta := fullJitter(2*spread) - spread
	u.schedule(base + delta)
}

func (u *Uploader) schedule(delay time.Duration) {
	u.mu.Lock()
	u.snapshot.NextAttempt = u.now().Add(delay)
	u.mu.Unlock()
}

func fullJitter(ceiling time.Duration) time.Duration {
	if ceiling <= 0 {
		return 0
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return ceiling / 2
	}
	return time.Duration(binary.LittleEndian.Uint64(raw[:]) % uint64(ceiling))
}

func retryAfter(err error) time.Duration {
	var apiErr *protocol.APIError
	if errors.As(err, &apiErr) {
		return apiErr.RetryAfter
	}
	return 0
}

func appVersion() string {
	if buildinfo.Version == "" {
		return "0.0.0-dev"
	}
	return buildinfo.Version
}
