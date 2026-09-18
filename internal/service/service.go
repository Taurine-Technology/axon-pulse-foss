// Package service composes the Pulse scheduler, probes, spool, uploader, and IPC.
package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Taurine-Technology/axon-contracts/gen/go/buildinfo"
	"github.com/Taurine-Technology/axon-pulse/internal/aggregate"
	"github.com/Taurine-Technology/axon-pulse/internal/ipc"
	"github.com/Taurine-Technology/axon-pulse/internal/measurement"
	"github.com/Taurine-Technology/axon-pulse/internal/prober"
	"github.com/Taurine-Technology/axon-pulse/internal/protocol"
	"github.com/Taurine-Technology/axon-pulse/internal/spool"
	"github.com/Taurine-Technology/axon-pulse/internal/state"
	"github.com/Taurine-Technology/axon-pulse/internal/support"
	pulseupdate "github.com/Taurine-Technology/axon-pulse/internal/update"
	"github.com/Taurine-Technology/axon-pulse/internal/uploader"
	"github.com/Taurine-Technology/axon-pulse/quality"
)

const (
	// lowBatteryDeferPct is the charge below which scheduled load is
	// deferred. Measuring on battery is fine; draining a nearly dead machine
	// is not. An unreadable charge level stays conservative.
	lowBatteryDeferPct = 20
	// updateDistributionBase is Axon's self-hosted distribution endpoint: a
	// Cloudflare R2 bucket behind a custom domain. Channels live under
	// pulse/{alpha,beta,main}/ and share one filename set, so promotion
	// between channels never changes a URL.
	updateDistributionBase = "https://dist.taurinetech.com/pulse/"

	// triggerScheduled is the cadence scheduler: gated and ledger-bound.
	triggerScheduled speedTrigger = 0
	// triggerManual is the device's own Run button: device-owned ceilings.
	triggerManual speedTrigger = 1
	// triggerController is a console operator's one-off: configured ceilings.
	triggerController speedTrigger = 2
)

type (
	// speedTrigger names who asked for a speed test; it selects the deferral,
	// ledger, and ceiling policy in runSpeedTestLocked.
	speedTrigger uint8

	Status struct {
		Version            string                        `json:"version"`
		State              string                        `json:"state"`
		Mode               string                        `json:"mode"`
		Connected          bool                          `json:"connected"`
		SensorID           string                        `json:"sensor_id,omitempty"`
		SiteID             string                        `json:"site_id,omitempty"`
		ControllerURL      string                        `json:"controller_url,omitempty"`
		ConfigVersion      int                           `json:"config_version"`
		SSIDConsent        bool                          `json:"ssid_consent"`
		SSIDPurgePending   bool                          `json:"ssid_purge_pending,omitempty"`
		PrivacyPurgeAction state.PrivacyPurgeAction      `json:"privacy_purge_action,omitempty"`
		SSIDRequested      bool                          `json:"ssid_requested"`
		PausedUntil        time.Time                     `json:"paused_until,omitzero"`
		RevokedAt          time.Time                     `json:"revoked_at,omitzero"`
		Spool              spool.Stats                   `json:"spool"`
		Upload             uploader.Snapshot             `json:"upload"`
		UptimeSeconds      int64                         `json:"uptime_seconds"`
		Live               []aggregate.Sample            `json:"live,omitempty"`
		Captive            bool                          `json:"captive"`
		LastSpeedTest      time.Time                     `json:"last_speed_test,omitzero"`
		NextSpeedTest      time.Time                     `json:"next_speed_test,omitzero"`
		LastSpeedTests     map[string]time.Time          `json:"last_speed_tests,omitzero"`
		NextSpeedTests     map[string]time.Time          `json:"next_speed_tests,omitzero"`
		SpeedProfiles      []protocol.SpeedProfileConfig `json:"speed_profiles"`
		// ManualTestEnvelopes reports the device-owned byte ceilings a
		// user-initiated test of each profile may use, which can exceed the
		// remotely scheduled profile sizes.
		ManualTestEnvelopes map[string]uint64 `json:"manual_test_envelope_bytes,omitempty"`
		// HouseholdMix is the mix the next household test reproduces and
		// HouseholdMixSource says whether it is the default, controller
		// configuration or a local override. HouseholdAvailable is false when
		// the connected controller does not support household tests yet.
		HouseholdMix       quality.HouseholdMix `json:"household_mix"`
		HouseholdMixSource string               `json:"household_mix_source"`
		HouseholdAvailable bool                 `json:"household_available"`
		DataBudgetBytes    uint64               `json:"data_budget_remaining_bytes"`
		DataBudgetLimit    uint64               `json:"data_budget_limit_bytes"`
		MonthlyBudgetBytes uint64               `json:"monthly_budget_remaining_bytes"`
		MonthlyBudgetLimit uint64               `json:"monthly_budget_limit_bytes"`
		// UpdateChannel is the release stream update checks follow.
		// UpdateChannelLocked is set when the service environment pins a
		// full index URL or APT owns updates, which the user cannot override.
		UpdateMethod        string `json:"update_method"`
		UpdateChannel       string `json:"update_channel"`
		UpdateChannelLocked bool   `json:"update_channel_locked,omitempty"`
	}

	LiveUpdate struct {
		State         string                      `json:"state"`
		Samples       []aggregate.Sample          `json:"samples"`
		Upload        uploader.Snapshot           `json:"upload"`
		Captive       bool                        `json:"captive"`
		SpeedProgress *protocol.SpeedTestProgress `json:"speed_progress,omitempty"`
		// UpdateProgress streams download progress while an update stages.
		// It is live-only and never persisted.
		UpdateProgress *UpdateProgress `json:"update_progress,omitempty"`
		// ConfigVersion and Version let a long-lived window notice that the
		// controller configuration or the service build changed underneath
		// it and refetch the full status, which live ticks deliberately omit.
		ConfigVersion int    `json:"config_version,omitempty"`
		Version       string `json:"version,omitempty"`
	}

	UpdateProgress struct {
		Version         string `json:"version,omitempty"`
		DownloadedBytes int64  `json:"downloaded_bytes"`
		TotalBytes      int64  `json:"total_bytes"`
		Done            bool   `json:"done,omitempty"`
	}

	UpdateStatus struct {
		UpdateMethod     string                 `json:"update_method"`
		CurrentVersion   string                 `json:"current_version"`
		AvailableVersion string                 `json:"available_version,omitempty"`
		Available        *pulseupdate.Available `json:"available,omitempty"`
		StagedPath       string                 `json:"staged_path,omitempty"`
		Restarting       bool                   `json:"restarting,omitempty"`
		CheckedAt        time.Time              `json:"checked_at,omitzero"`
		Channel          string                 `json:"channel,omitempty"`
		ChannelLocked    bool                   `json:"channel_locked,omitempty"`
		// CheckError carries a failed check that followed a successful
		// stream change, so the change itself is never reported as failed.
		CheckError string `json:"check_error,omitempty"`
	}

	probeEngine interface {
		Latency(context.Context, protocol.SensorConfig, string) []aggregate.Sample
		DNS(context.Context, protocol.SensorConfig) []protocol.DNSCheck
		HTTP(context.Context, protocol.SensorConfig, string) []protocol.HTTPCheck
	}

	linkCollector interface {
		Collect(context.Context, time.Time, bool) protocol.LinkContext
	}

	speedRunner interface {
		RunTest(context.Context, protocol.TrafficContext, uint64, protocol.SpeedProfileConfig, measurement.HouseholdRequest) (protocol.SpeedTest, error)
	}

	updateManager interface {
		Check(context.Context, string, string) (pulseupdate.Available, error)
		Stage(context.Context, pulseupdate.Available, string) (string, error)
	}

	Service struct {
		store        *state.Store
		spool        *spool.Spool
		clearSpool   func(context.Context) error
		aggregator   *aggregate.Aggregator
		prober       probeEngine
		collector    linkCollector
		speed        speedRunner
		crossTraffic func(context.Context, time.Duration) (float64, error)
		uploader     *uploader.Uploader
		client       *protocol.Client
		logger       *slog.Logger
		logCloser    io.Closer
		ipc          *ipc.Server
		started      time.Time
		stateDir     string
		updates      updateManager
		// newUpdateManager builds the manager for an index URL; a stream
		// change swaps the manager without restarting the service.
		newUpdateManager func(indexURL string) updateManager
		activate         func(context.Context, string, string, string) error
		restartDelay     time.Duration
		shutdownWait     time.Duration
		updateDrainWait  time.Duration
		activity         activityGate
		wg               sync.WaitGroup
		unsafeDrains     atomic.Int32
		enrollRun        sync.Mutex
		updateRun        sync.Mutex
		modeRun          sync.RWMutex

		mu               sync.Mutex
		probeBusy        bool
		dnsBusy          bool
		httpBusy         bool
		uploadBusy       bool
		speedBusy        bool
		captive          bool
		lastProbe        time.Time
		lastDNS          time.Time
		lastHTTP         time.Time
		nextSpeedAttempt time.Time
		speedFailures    int
		flushMinute      time.Time
		lifecycleState   string
		watchers         map[uint64]chan any
		nextWatcherID    uint64
		updateMethod     string
		updateStatus     UpdateStatus
		// APT-managed builds watch their installed executable; see package_restart.go.
		packagePath      string
		packageInfo      os.FileInfo
		packageCheckedAt time.Time
		packageRestart   bool
		shutdown         chan struct{}
		shutdownOnce     sync.Once
		updateBusy       bool
		lastUpdateCheck  time.Time
	}

	speedDeferredError struct{ reason string }
)

var (
	ErrUnsafeShutdown = errors.New("service still has active work; dependent resources must remain open")
)

func Open(stateDir, socketPath string, logger *slog.Logger) (*Service, error) {
	if logger == nil {
		logger = slog.Default()
	}
	store, err := state.Open(stateDir)
	if err != nil {
		return nil, err
	}
	queue, err := spool.Open(stateDir+string(os.PathSeparator)+"spool.db", spool.Options{})
	if err != nil {
		return nil, err
	}
	logger, logCloser, err := support.NewRotatingLogger(stateDir, logger)
	if err != nil {
		_ = queue.Close()
		return nil, fmt.Errorf("open bounded local log: %w", err)
	}
	client := protocol.NewClient()
	service := &Service{
		store: store, spool: queue, aggregator: aggregate.New(), prober: prober.New(),
		collector: &measurement.Collector{}, speed: measurement.SpeedRunner{}, crossTraffic: measurement.MeasureCrossTraffic, client: client, logger: logger, logCloser: logCloser,
		started: time.Now(), stateDir: stateDir, flushMinute: time.Now().Truncate(time.Minute),
		watchers:     make(map[uint64]chan any),
		updateMethod: pulseupdate.InstallMethod(),
		shutdown:     make(chan struct{}),
	}
	service.speed = measurement.SpeedRunner{Progress: service.broadcastSpeedProgress}
	service.clearSpool = queue.Clear
	service.newUpdateManager = func(indexURL string) updateManager {
		return pulseupdate.Manager{IndexURL: indexURL, Progress: service.broadcastUpdateProgress}
	}
	_, indexURL, _ := resolveUpdateSource(store.Snapshot().UpdateChannel)
	service.updates = service.newUpdateManager(indexURL)
	service.activate = func(ctx context.Context, staged, executable, _ string) error {
		return pulseupdate.Activate(ctx, staged, executable)
	}
	if updateKind() == "desktop" {
		service.activate = pulseupdate.ActivateDesktop
	}
	service.restartDelay = time.Second
	service.shutdownWait = 30 * time.Second
	service.updateDrainWait = 30 * time.Second
	service.updateStatus.CurrentVersion = serviceVersion()
	service.updateStatus.UpdateMethod = service.updateMethod
	service.watchInstalledPackage()
	service.uploader = uploader.New(store, queue, client, logger)
	service.ipc = &ipc.Server{Path: socketPath, Handler: service}
	return service, nil
}

func (s *Service) Close() error {
	if active := s.unsafeDrains.Load(); active > 0 {
		return fmt.Errorf("%w: %d drain(s) pending", ErrUnsafeShutdown, active)
	}
	var joined error
	if s.ipc != nil {
		if err := s.ipc.Close(); err != nil {
			joined = errors.Join(joined, fmt.Errorf("close IPC server: %w", err))
		}
	}
	if s.spool != nil {
		if err := s.spool.Close(); err != nil {
			joined = errors.Join(joined, fmt.Errorf("close telemetry spool: %w", err))
		}
	}
	if joined != nil && s.logger != nil {
		s.logger.Error("service_resource_close_failed", "error", joined)
	}
	if s.logCloser != nil {
		if err := s.logCloser.Close(); err != nil {
			joined = errors.Join(joined, fmt.Errorf("close service log: %w", err))
		}
	}
	return joined
}

func (s *Service) Run(ctx context.Context) (runErr error) {
	if err := s.ipc.Listen(); err != nil {
		return err
	}
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	defer func() {
		workersDone := make(chan struct{})
		go func() {
			s.wg.Wait()
			close(workersDone)
		}()
		timer := time.NewTimer(s.shutdownWait)
		defer timer.Stop()
		select {
		case <-workersDone:
		case <-timer.C:
			s.trackUnsafeDrain(workersDone)
			s.logger.Error("service_worker_drain_timed_out", "timeout", s.shutdownWait)
			runErr = errors.Join(runErr, fmt.Errorf("drain service workers: timed out after %s", s.shutdownWait))
			return
		}
		if active := s.unsafeDrains.Load(); active > 0 {
			s.logger.Error("shutdown_flush_skipped_for_active_work", "active_drains", active)
			runErr = errors.Join(runErr, ErrUnsafeShutdown)
			return
		}
		flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.modeRun.Lock()
		flushErr := s.flushShutdown(flushCtx, time.Now(), s.store.Snapshot().Mode == state.ModeStandalone)
		s.modeRun.Unlock()
		if flushErr != nil {
			s.logger.Error("shutdown_flush_failed", "error", flushErr)
			runErr = errors.Join(runErr, fmt.Errorf("flush telemetry during shutdown: %w", flushErr))
		}
	}()
	ipcErrors := make(chan error, 1)
	go func() { ipcErrors <- s.ipc.Serve(runCtx) }()
	s.logger.Info("pulsed_started", "socket", s.ipc.Path, "state", s.lifecycle(time.Now()))
	if s.store.Snapshot().SSIDPurgePending {
		if err := s.completeSSIDPurge(runCtx); err != nil {
			s.logger.Error("ssid_privacy_purge_failed", "error", err)
		}
	}
	s.uploader.Force()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			cancelRun()
			closeErr := s.ipc.Close()
			serveErr := <-ipcErrors
			s.trackIPCDrain(serveErr)
			return errors.Join(closeErr, serveErr)
		case <-s.shutdown:
			cancelRun()
			closeErr := s.ipc.Close()
			serveErr := <-ipcErrors
			s.trackIPCDrain(serveErr)
			return errors.Join(closeErr, serveErr)
		case err := <-ipcErrors:
			cancelRun()
			s.trackIPCDrain(err)
			if err != nil {
				return err
			}
			return nil
		case now := <-ticker.C:
			s.tick(runCtx, now)
		}
	}
}

func (s *Service) trackIPCDrain(err error) {
	if errors.Is(err, ipc.ErrDrainTimeout) {
		s.trackUnsafeDrain(s.ipc.Drained())
	}
}

func (s *Service) trackUnsafeDrain(done <-chan struct{}) {
	s.unsafeDrains.Add(1)
	go func() {
		<-done
		s.unsafeDrains.Add(-1)
	}()
}

func (s *Service) tick(ctx context.Context, now time.Time) {
	defer s.broadcastLive(now)
	if s.store.Snapshot().SSIDPurgePending {
		if err := s.completeSSIDPurge(ctx); err != nil {
			s.logger.Error("ssid_privacy_purge_failed", "error", err)
		}
		return
	}
	s.startAutomaticUpdate(ctx, now)
	s.checkInstalledPackage(ctx, now)
	releaseActivity, admitted := s.activity.begin()
	if !admitted {
		return
	}
	s.modeRun.RLock()
	current := s.store.Snapshot()
	if !current.Operational() {
		s.aggregator.Reset()
		s.modeRun.RUnlock()
		releaseActivity()
		s.startUpload(ctx)
		return
	}
	if current.Paused(now) {
		if s.flush(ctx, now, current.Mode == state.ModeStandalone) {
			s.flushMinute = now.Truncate(time.Minute)
		}
		s.modeRun.RUnlock()
		releaseActivity()
		s.startUpload(ctx)
		return
	}
	if now.Truncate(time.Minute).After(s.flushMinute) {
		if s.flush(ctx, now, current.Mode == state.ModeStandalone) {
			s.flushMinute = now.Truncate(time.Minute)
		}
	}
	config, controllerURL := effectiveMeasurementConfig(current)
	var controllerRequest *protocol.SpeedTestRequest
	controllerProfile := ""
	if current.Mode == state.ModeConnected {
		controllerRequest = s.uploader.PendingSpeedTestRequest()
		if controllerRequest != nil {
			profile, ok := configuredSpeedProfile(config, controllerRequest.Profile)
			reason := validateControllerSpeedRequest(*controllerRequest, now)
			if !ok || !profile.Enabled {
				reason = "profile_disabled"
			}
			if reason != "" {
				s.uploader.ClearSpeedTestRequest(controllerRequest.Nonce)
				s.resolveControllerSpeedTest(ctx, now, current.Mode == state.ModeStandalone, controllerRequest.Profile, "", &speedDeferredError{reason: reason})
				controllerRequest = nil
			} else {
				// An operator's one-off is explicit intent, exactly like the
				// device's own Run button: it ignores cadence spacing and the
				// failure backoff. It still waits for a busy sensor, and its
				// signed expiry bounds how long it may wait.
				controllerProfile = profile.Name
			}
		}
	}
	s.mu.Lock()
	probeDue := !s.probeBusy && now.Sub(s.lastProbe) >= time.Duration(config.ProbeIntervalSeconds)*time.Second
	dnsDue := !s.dnsBusy && now.Sub(s.lastDNS) >= time.Duration(config.DNSIntervalSeconds)*time.Second
	httpDue := !s.httpBusy && now.Sub(s.lastHTTP) >= time.Duration(config.HTTPIntervalSeconds)*time.Second
	speedProfile := ""
	controllerSpeed := false
	if controllerProfile != "" && !s.speedBusy && !s.uploadBusy && !s.updateBusy {
		speedProfile, controllerSpeed = controllerProfile, true
	} else if now.Sub(s.started) >= 5*time.Minute && !s.speedBusy && !s.uploadBusy && !s.updateBusy && !now.Before(s.nextSpeedAttempt) {
		for _, profile := range config.SpeedProfiles {
			last := current.LastSpeedTests[profile.Name]
			if profile.Enabled && measurement.SpeedProfileDue(now, last, profile.Windows, profile.CadenceMinutes, current.SensorUID+":"+profile.Name) &&
				now.Sub(last) >= time.Duration(profile.MinSpacingMinutes)*time.Minute {
				speedProfile = profile.Name
				break
			}
		}
	}
	speedDue := speedProfile != ""
	if probeDue {
		s.probeBusy, s.lastProbe = true, now
	}
	if dnsDue {
		s.dnsBusy, s.lastDNS = true, now
	}
	if httpDue {
		s.httpBusy, s.lastHTTP = true, now
	}
	if speedDue {
		s.speedBusy = true
	}
	s.mu.Unlock()
	if speedDue && controllerSpeed {
		accepted, err := s.store.AcceptSpeedTestRequest(controllerRequest.Nonce, time.Unix(controllerRequest.ExpiresAt, 0), now)
		if err != nil || !accepted {
			s.clearBusy("speed")
			speedDue = false
			if err != nil {
				s.logger.Warn("controller_speed_test_accept_failed", "error", err)
			} else {
				s.uploader.ClearSpeedTestRequest(controllerRequest.Nonce)
				s.resolveControllerSpeedTest(ctx, now, current.Mode == state.ModeStandalone, controllerProfile, "", &speedDeferredError{reason: "replayed_nonce"})
			}
		} else {
			// The nonce is spent now; the accepted/rejected verdict follows
			// the run itself so the console learns what actually happened.
			s.uploader.ClearSpeedTestRequest(controllerRequest.Nonce)
		}
	}
	var producers sync.WaitGroup
	if probeDue {
		producers.Add(1)
		s.wg.Go(func() {
			defer producers.Done()
			defer s.clearBusy("probe")
			for _, sample := range s.prober.Latency(ctx, config, controllerURL) {
				s.aggregator.AddSample(sample)
			}
		})
	}
	if dnsDue {
		producers.Add(1)
		s.wg.Go(func() {
			defer producers.Done()
			defer s.clearBusy("dns")
			for _, check := range s.prober.DNS(ctx, config) {
				if err := s.aggregator.AddDNS(check); err != nil {
					s.logger.Error("dns_result_rejected", "error", err)
				}
			}
		})
	}
	if httpDue {
		producers.Add(1)
		s.wg.Go(func() {
			defer producers.Done()
			defer s.clearBusy("http")
			checks := s.prober.HTTP(ctx, config, controllerURL)
			captive := false
			for _, check := range checks {
				if err := s.aggregator.AddHTTP(check); err != nil {
					s.logger.Error("http_result_rejected", "error", err)
				}
				captive = captive || check.Captive
			}
			s.setCaptive(captive, time.Now())
			shareSSID := config.SendSSID && current.SSIDConsent
			link := s.collector.Collect(ctx, time.Now(), shareSSID)
			if !shareSSID {
				link.SSID = ""
			}
			if config.SendNetworkIdentity && current.Mode == state.ModeConnected && current.NetworkIdentityKey != "" {
				applyPseudonymousLinkIdentity(&link, current.NetworkIdentityKey)
			}
			link.RawNetworkIdentity, link.RawFirstHop = "", ""
			if err := s.aggregator.AddLink(link); err != nil {
				s.logger.Error("link_context_rejected", "error", err)
			}
			if link.GatewayChanged {
				_ = s.aggregator.AddEvent(protocol.Event{Timestamp: link.Timestamp, Type: "gateway_changed", Detail: map[string]any{}})
			}
		})
	}
	if speedDue {
		producers.Add(1)
		s.wg.Go(func() {
			defer producers.Done()
			defer s.clearBusy("speed")
			trigger := triggerScheduled
			if controllerSpeed {
				trigger = triggerController
			}
			result, err := s.runSpeedTestLocked(ctx, trigger, speedProfile)
			if controllerSpeed {
				s.resolveControllerSpeedTest(ctx, time.Now(), current.Mode == state.ModeStandalone, speedProfile, result.MeasurementID, err)
			}
			if err != nil {
				var deferred *speedDeferredError
				if !errors.As(err, &deferred) {
					s.deferSpeedFailure(time.Now())
				}
				s.logger.Info("scheduled_speed_test_deferred", "reason", err, "controller_requested", controllerSpeed)
			}
		})
	}
	// A mode change waits until every producer selected from this snapshot has
	// published, including a speed test. Once a writer is pending, the next tick
	// intentionally waits instead of resnapshotting under a different mode.
	s.releaseModeAfter(&producers, releaseActivity, probeDue || dnsDue || httpDue || speedDue)
	s.startUpload(ctx)
}

func (s *Service) releaseModeAfter(producers *sync.WaitGroup, releaseActivity func(), asynchronous bool) {
	if !asynchronous {
		s.modeRun.RUnlock()
		releaseActivity()
		return
	}
	s.wg.Go(func() {
		producers.Wait()
		s.modeRun.RUnlock()
		releaseActivity()
	})
}

func (s *Service) startAutomaticUpdate(ctx context.Context, now time.Time) {
	if s.updateMethod == "apt" || !automaticUpdatesEnabled() || now.Sub(s.started) < 10*time.Minute {
		return
	}
	s.mu.Lock()
	due := !s.updateBusy && (s.lastUpdateCheck.IsZero() || now.Sub(s.lastUpdateCheck) >= 24*time.Hour) && !s.speedBusy && !s.uploadBusy
	if due {
		s.updateBusy = true
		s.lastUpdateCheck = now
		s.wg.Add(1)
	}
	s.mu.Unlock()
	if !due {
		return
	}
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			s.updateBusy = false
			s.mu.Unlock()
		}()
		result, ipcErr := s.HandleIPC(ctx, ipc.Request{Method: "check_update"})
		if ipcErr != nil {
			s.logger.Debug("automatic_update_check_failed", "error", ipcErr)
			return
		}
		status, ok := result.(UpdateStatus)
		if !ok || status.AvailableVersion == "" || (status.StagedPath != "" && status.Available != nil && status.AvailableVersion == status.Available.Version) {
			return
		}
		if _, ipcErr := s.HandleIPC(ctx, ipc.Request{Method: "stage_update"}); ipcErr != nil {
			s.logger.Warn("automatic_update_stage_failed", "error", ipcErr)
		}
	}()
}

func (s *Service) broadcastLive(now time.Time) {
	state := s.lifecycle(now)
	s.trackLifecycle(now, state)
	update := LiveUpdate{
		State:         state,
		Upload:        s.uploader.Snapshot(),
		ConfigVersion: s.store.Snapshot().ConfigVersion,
		Version:       serviceVersion(),
	}
	live := s.aggregator.Live()
	if len(live) > 0 {
		update.Samples = append([]aggregate.Sample(nil), live[max(0, len(live)-8):]...)
	}
	s.mu.Lock()
	update.Captive = s.captive
	for _, watcher := range s.watchers {
		select {
		case watcher <- update:
		default:
		}
	}
	s.mu.Unlock()
}

// streamTrafficCheckProgress keeps the UI alive during the pre-test
// cross-traffic sample, which otherwise reads as dead air between pressing
// Start and the first measured byte.
func (s *Service) streamTrafficCheckProgress(done <-chan struct{}, started time.Time, profileName string) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	emit := func() {
		s.broadcastSpeedProgress(protocol.SpeedTestProgress{
			MeasurementID: "preflight", Profile: profileName, Phase: "traffic_check",
			ElapsedMS: time.Since(started).Milliseconds(),
		})
	}
	emit()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			emit()
		}
	}
}

// broadcastSpeedProgress fans transient in-test snapshots out to live
// watchers. Progress is UI-only: it is never aggregated, spooled, or uploaded.
func (s *Service) broadcastSpeedProgress(progress protocol.SpeedTestProgress) {
	update := LiveUpdate{State: s.lifecycle(time.Now()), Upload: s.uploader.Snapshot(), SpeedProgress: &progress}
	s.mu.Lock()
	update.Captive = s.captive
	for _, watcher := range s.watchers {
		select {
		case watcher <- update:
		default:
		}
	}
	s.mu.Unlock()
}

// broadcastUpdateProgress forwards staging progress to live watchers. The
// update manager already throttles reports by byte count, so every call is
// worth sending; the version comes from the update being staged.
func (s *Service) broadcastUpdateProgress(downloaded, total int64) {
	update := LiveUpdate{State: s.lifecycle(time.Now()), Upload: s.uploader.Snapshot()}
	s.mu.Lock()
	version := ""
	if s.updateStatus.Available != nil {
		version = s.updateStatus.Available.Version
	}
	update.Captive = s.captive
	update.UpdateProgress = &UpdateProgress{Version: version, DownloadedBytes: downloaded, TotalBytes: total, Done: total > 0 && downloaded >= total}
	for _, watcher := range s.watchers {
		select {
		case watcher <- update:
		default:
		}
	}
	s.mu.Unlock()
}

func (s *Service) trackLifecycle(now time.Time, current string) {
	s.mu.Lock()
	previous := s.lifecycleState
	s.lifecycleState = current
	s.mu.Unlock()
	if previous == "" || previous == current {
		return
	}
	eventType := ""
	switch {
	case current == "offline":
		eventType = "offline_start"
	case previous == "offline" && current == "active":
		eventType = "offline_end"
	case current == "paused":
		eventType = "paused"
	case previous == "paused":
		eventType = "resumed"
	}
	if eventType != "" {
		_ = s.aggregator.AddEvent(protocol.Event{
			Timestamp: now.Unix(), Type: eventType,
			Detail: map[string]any{"previous": previous, "current": current},
		})
	}
}

func (s *Service) subscribe() ipc.Stream {
	s.mu.Lock()
	s.nextWatcherID++
	id := s.nextWatcherID
	channel := make(chan any, 1)
	s.watchers[id] = channel
	s.mu.Unlock()
	return ipc.Stream{
		C: channel,
		Close: func() {
			s.mu.Lock()
			if existing, ok := s.watchers[id]; ok {
				delete(s.watchers, id)
				close(existing)
			}
			s.mu.Unlock()
		},
	}
}

func (s *Service) flush(ctx context.Context, now time.Time, localOnly bool) bool {
	// Hold the lifecycle mutex through metric extraction. A new producer cannot
	// start, and a completing producer does not clear probeBusy until after all
	// of its samples have been added, so no finalized minute can be recreated.
	s.mu.Lock()
	if s.probeBusy {
		s.mu.Unlock()
		s.appendFlushed(ctx, s.aggregator.FlushPending(), nil, localOnly)
		return false
	}
	records, err := s.aggregator.Flush(now)
	s.mu.Unlock()
	s.appendFlushed(ctx, records, err, localOnly)
	return true
}

func (s *Service) flushShutdown(ctx context.Context, now time.Time, localOnly bool) error {
	records, err := s.aggregator.Shutdown(now)
	if err != nil {
		return err
	}
	return s.persistFlushed(ctx, records, localOnly)
}

// flushForUpdate persists all complete and non-metric telemetry while keeping
// the current partial minute live. If activation fails, normal collection can
// resume without silently losing that in-memory interval.
func (s *Service) flushForUpdate(ctx context.Context, now time.Time, localOnly bool) error {
	records, err := s.aggregator.Flush(now)
	if err != nil {
		return err
	}
	return s.persistFlushed(ctx, records, localOnly)
}

func (s *Service) completeSSIDPurge(ctx context.Context) error {
	s.modeRun.Lock()
	defer s.modeRun.Unlock()
	return s.completeSSIDPurgeLocked(ctx)
}

// completeSSIDPurgeLocked requires modeRun for writing. The durable pending
// marker was committed before this point, so every failure leaves uploads
// blocked and the cleanup safe to retry after a restart.
func (s *Service) completeSSIDPurgeLocked(ctx context.Context) error {
	current := s.store.Snapshot()
	s.aggregator.Reset()
	if err := s.clearSpool(ctx); err != nil {
		return fmt.Errorf("clear SSID-bearing telemetry: %w", err)
	}
	switch current.PrivacyPurgeAction {
	case state.PrivacyPurgeDisconnect:
		if err := s.store.WipeCredentials(false); err != nil {
			return fmt.Errorf("wipe disconnected sensor credentials: %w", err)
		}
	case state.PrivacyPurgeRevocation:
		if err := s.store.WipeCredentials(true); err != nil {
			return fmt.Errorf("wipe revoked sensor credentials: %w", err)
		}
	}
	if err := s.store.CompleteSSIDPurge(); err != nil {
		return fmt.Errorf("complete SSID privacy purge: %w", err)
	}
	s.uploader.ClearSpeedTestRequests()
	s.uploader.ClearPrivacyBlock()
	return nil
}

func (s *Service) appendFlushed(ctx context.Context, records []spool.Record, err error, localOnly bool) {
	if err != nil {
		s.logger.Error("aggregate_flush_failed", "error", err)
		return
	}
	if err := s.persistFlushed(ctx, records, localOnly); err != nil {
		s.logger.Error("spool_append_failed", "error", err)
	}
}

func (s *Service) persistFlushed(ctx context.Context, records []spool.Record, localOnly bool) error {
	// The aggregator accepts pre-epoch samples (RTC-less hardware before time
	// sync), but the spool rejects non-positive timestamps for the whole batch
	// and a rejected batch would be restored and retried forever. Drop the
	// poison records here instead of bricking every future flush.
	valid := records[:0]
	for _, record := range records {
		if record.Timestamp > 0 {
			valid = append(valid, record)
		}
	}
	if dropped := len(records) - len(valid); dropped > 0 {
		s.logger.Warn("pre_epoch_records_dropped", "count", dropped)
	}
	records = valid
	if len(records) == 0 {
		return nil
	}
	var dropped int64
	var err error
	if localOnly {
		dropped, err = s.spool.AppendLocal(ctx, records)
	} else {
		dropped, err = s.spool.AppendMinute(ctx, records)
	}
	if err != nil {
		s.aggregator.Restore(records)
		return err
	}
	if dropped > 0 {
		s.logger.Warn("spool_records_dropped", "count", dropped)
	}
	s.uploader.Force()
	return nil
}

func (s *Service) startUpload(ctx context.Context) {
	releaseActivity, admitted := s.activity.begin()
	if !admitted {
		return
	}
	s.mu.Lock()
	if s.uploadBusy || s.speedBusy {
		s.mu.Unlock()
		releaseActivity()
		return
	}
	s.uploadBusy = true
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		defer releaseActivity()
		defer s.clearBusy("upload")
		s.modeRun.RLock()
		uploadErr := s.uploader.Tick(ctx)
		s.modeRun.RUnlock()
		if uploadErr != nil && !errors.Is(uploadErr, protocol.ErrRevoked) {
			s.logger.Warn("upload_tick_failed", "error", uploadErr)
		}
		if s.store.Snapshot().SSIDPurgePending {
			if err := s.completeSSIDPurge(ctx); err != nil {
				s.logger.Error("revocation_privacy_purge_failed", "error", err)
			}
		}
	}()
}

// tickUploadNow runs one synchronous upload tick under the same one-active-test
// guard as startUpload, so an immediate upload (e.g. the enrollment heartbeat)
// cannot overlap a running speed test and contaminate its traffic. When busy it
// returns nil; the forced upload runs on the next scheduler tick instead.
func (s *Service) tickUploadNow(ctx context.Context) error {
	releaseActivity, admitted := s.activity.begin()
	if !admitted {
		return errActivityQuiescing
	}
	defer releaseActivity()
	s.mu.Lock()
	if s.uploadBusy || s.speedBusy {
		s.mu.Unlock()
		return nil
	}
	s.uploadBusy = true
	s.mu.Unlock()
	defer s.clearBusy("upload")
	s.modeRun.RLock()
	uploadErr := s.uploader.Tick(ctx)
	s.modeRun.RUnlock()
	if s.store.Snapshot().SSIDPurgePending {
		cleanupErr := s.completeSSIDPurge(ctx)
		return errors.Join(uploadErr, cleanupErr)
	}
	return uploadErr
}

func (s *Service) clearBusy(kind string) {
	s.mu.Lock()
	switch kind {
	case "probe":
		s.probeBusy = false
	case "dns":
		s.dnsBusy = false
	case "http":
		s.httpBusy = false
	case "upload":
		s.uploadBusy = false
	case "speed":
		s.speedBusy = false
	case "update":
		s.updateBusy = false
	}
	s.mu.Unlock()
}

func (s *Service) setCaptive(captive bool, now time.Time) {
	s.mu.Lock()
	wasCaptive := s.captive
	s.captive = captive
	s.mu.Unlock()
	if captive && !wasCaptive {
		_ = s.aggregator.AddEvent(protocol.Event{Timestamp: now.Unix(), Type: "captive_detected", Detail: map[string]any{}})
	}
}

func (s *Service) runSpeedTest(ctx context.Context, profileName string) (protocol.SpeedTest, error) {
	releaseActivity, admitted := s.activity.begin()
	if !admitted {
		return protocol.SpeedTest{}, errActivityQuiescing
	}
	defer releaseActivity()
	s.modeRun.RLock()
	defer s.modeRun.RUnlock()
	// A user-initiated test is explicit intent: run it now and record the live
	// conditions (cross traffic, battery, metering) as annotations instead of
	// deferring. Only scheduled tests keep their gates.
	return s.runSpeedTestLocked(ctx, triggerManual, profileName)
}

// resolveControllerSpeedTest reports a one-off directive's outcome. The
// controller resolves its delivered request from exactly one of these events,
// so the verdict is emitted once, after the run, with the concrete reason on
// failure instead of a bare acceptance followed by silence. A nil err is an
// acceptance and names the measurement it produced, so the controller can
// join the directive to its result; a speedDeferredError names the reason;
// anything else is an endpoint or network failure. The verdict is flushed
// and shipped at once rather than on the next minute's tick, so a crash
// cannot lose it and the console learns the outcome with the result.
func (s *Service) resolveControllerSpeedTest(ctx context.Context, now time.Time, localOnly bool, profileName, measurementID string, err error) {
	event := protocol.Event{Timestamp: now.Unix(), Type: "controller_speed_test_accepted", Detail: map[string]any{"profile": profileName}}
	if measurementID != "" && err == nil {
		event.Detail["measurement_id"] = measurementID
	}
	if err != nil {
		reason := "endpoint_or_network_failure"
		var deferred *speedDeferredError
		if errors.As(err, &deferred) {
			reason = deferred.reason
		}
		event.Type = "controller_speed_test_rejected"
		event.Detail["reason"] = reason
	}
	_ = s.aggregator.AddEvent(event)
	s.flush(ctx, now, localOnly)
	s.uploader.Force()
}

// runSpeedTestLocked requires modeRun to be held for reading. Scheduled tests
// inherit the exact mode/config snapshot that selected them; manual tests use
// runSpeedTest, which acquires the same lifecycle lock.
//
// The trigger decides policy. A scheduled run defers on cross traffic,
// metering, low battery, and exhausted ledgers. A manual run (device UI) and a
// controller one-off (console operator) are both explicit intent: they run
// now, annotate the live conditions, and may overdraw the ledgers. They differ
// only in ceilings: the device owns a manual run's, remote config owns the
// operator's, so remote policy still bounds what a console click can spend.
func (s *Service) runSpeedTestLocked(ctx context.Context, trigger speedTrigger, profileName string) (protocol.SpeedTest, error) {
	manual := trigger == triggerManual
	override := trigger != triggerScheduled
	now := time.Now()
	current := s.store.Snapshot()
	if !current.Operational() {
		return protocol.SpeedTest{}, errors.New("choose local or connected mode before running a test")
	}
	config, _ := effectiveMeasurementConfig(current)
	if profileName == "" {
		profileName = defaultSpeedProfile(config)
	}
	// The effective configuration decides which profiles exist: a controller
	// that does not speak the household contract never lists it, so its
	// ingest is never sent a block it would reject.
	profile, ok := configuredSpeedProfile(config, profileName)
	if !ok && manual && (profileName == protocol.ProfileContent || profileName == protocol.ProfileCapacity) {
		// The legacy pair is accepted by every controller, so a user may
		// still run either by hand even when it is no longer scheduled.
		// Household and saturation stay gated by the effective config.
		profile, ok = protocol.DeviceSpeedProfile(profileName)
	}
	if !ok {
		return protocol.SpeedTest{}, fmt.Errorf("unknown speed-test profile %q", profileName)
	}
	if manual {
		// How a user-initiated test measures is device-owned: use the local
		// adaptive profile ceilings even when remote config schedules smaller
		// background tests. Total spend stays bound by the config's daily and
		// monthly ledgers, which remote policy does control.
		if local, ok := protocol.DeviceSpeedProfile(profileName); ok {
			profile = local
		}
	}
	household := measurement.HouseholdRequest{}
	household.Mix, household.Source = protocol.EffectiveHouseholdMix(current.HouseholdMix, config.HouseholdMix)
	link := s.collector.Collect(ctx, now, false)
	// Manual runs no longer gate on cross traffic, so a short annotation
	// sample beats ten silent seconds between click and first byte.
	sampleWindow := 10 * time.Second
	// Preflight progress streams only for explicitly requested tests. A
	// scheduled test may still defer after its sample; streaming its preflight
	// and then never emitting a terminal event would leave live viewers locked
	// on a test that never happened.
	var sampleDone chan struct{}
	if override {
		sampleWindow = 3 * time.Second
		sampleDone = make(chan struct{})
		go s.streamTrafficCheckProgress(sampleDone, now, profileName)
	}
	crossTraffic, trafficErr := s.crossTraffic(ctx, sampleWindow)
	if sampleDone != nil {
		close(sampleDone)
	}
	if trafficErr != nil {
		s.logger.Debug("cross_traffic_unavailable", "error", trafficErr)
	}
	traffic := protocol.TrafficContext{
		CrossTrafficMbps: crossTraffic, Metered: link.Metered, MeteredAvailable: link.Availability.Metered,
		OnBattery: link.OnBattery, PowerAvailable: link.Availability.Power, BatteryPct: link.BatteryPct, Override: override,
	}
	traffic.Contaminated = crossTraffic > config.SpeedTestCrossTrafficMbps
	reason := speedTestDeferral(traffic, profile)
	dailyRemaining, monthlyRemaining := s.store.RemainingDataBudgetsFor(now, config.DailyDataBudgetMB, config.MonthlyDataBudgetMB)
	remaining := min(dailyRemaining, monthlyRemaining)
	// A user or operator may knowingly spend past the ledgers; never stop an
	// explicitly requested test. The overdraw is still charged, so scheduled
	// tests stay deferred until the ledger rolls over.
	if budgetReason := speedBudgetReason(dailyRemaining, monthlyRemaining); budgetReason != "" && !override {
		reason = budgetReason
	}
	if reason != "" {
		s.deferSpeed(now, reason, crossTraffic)
		return protocol.SpeedTest{}, &speedDeferredError{reason: reason}
	}
	envelope := profile.MaxDownloadBytes + profile.MaxUploadBytes
	runBytes := remaining
	reservation := min(envelope, remaining)
	var reserveErr error
	if override {
		runBytes = envelope
		reservation = envelope
		reserveErr = s.store.ReserveSpeedTestOverdraw(now, reservation, config.DailyDataBudgetMB, config.MonthlyDataBudgetMB)
	} else {
		reserveErr = s.store.ReserveSpeedTestProfileFor(now, reservation, config.DailyDataBudgetMB, config.MonthlyDataBudgetMB)
	}
	// The runner emits a terminal progress event on every path it reaches;
	// failures before it starts (reserve errors, pre-runner validation) must
	// emit one too or live viewers stay locked on the preflight phase.
	abortPreflight := func() {
		if override {
			s.broadcastSpeedProgress(protocol.SpeedTestProgress{MeasurementID: "preflight", Profile: profileName, Phase: "aborted", Done: true})
		}
	}
	if reserveErr != nil {
		abortPreflight()
		return protocol.SpeedTest{}, fmt.Errorf("reserve speed-test data budget: %w", reserveErr)
	}
	result, err := s.speed.RunTest(ctx, traffic, runBytes, profile, household)
	if err != nil && result.Timestamp == 0 {
		abortPreflight()
	}
	bytesUsed := speedTestBytes(result)
	if reconcileErr := s.store.ReconcileSpeedTestProfile(now, profile.Name, reservation, bytesUsed, err == nil); reconcileErr != nil {
		return protocol.SpeedTest{}, fmt.Errorf("reconcile speed-test data budget: %w", reconcileErr)
	}
	if err != nil {
		return result, err
	}
	result.ExperienceEligible, result.ExclusionReason = experienceEligibility(result, traffic, profile)
	if err := s.aggregator.AddSpeedTest(result); err != nil {
		return protocol.SpeedTest{}, err
	}
	s.flush(ctx, time.Now(), current.Mode == state.ModeStandalone)
	s.uploader.Force()
	s.mu.Lock()
	s.speedFailures = 0
	s.nextSpeedAttempt = time.Time{}
	s.mu.Unlock()
	s.logger.Info("speed_test_completed", "profile", profile.Name, "download_mbps", result.Download.ThroughputMbps, "upload_mbps", result.Upload.ThroughputMbps, "grade", result.Responsiveness.OverallGrade, "bytes", bytesUsed)
	return result, nil
}

func (s *Service) deferSpeed(now time.Time, reason string, crossTraffic float64) {
	s.mu.Lock()
	s.nextSpeedAttempt = now.Add(5 * time.Minute)
	s.mu.Unlock()
	_ = s.aggregator.AddEvent(protocol.Event{Timestamp: now.Unix(), Type: "speed_test_deferred", Detail: map[string]any{"reason": reason, "cross_traffic_mbps": crossTraffic}})
}

func (s *Service) deferSpeedFailure(now time.Time) {
	s.mu.Lock()
	s.speedFailures++
	delay := min(5*time.Minute*time.Duration(1<<min(s.speedFailures-1, 4)), time.Hour)
	s.nextSpeedAttempt = now.Add(delay)
	s.mu.Unlock()
	_ = s.aggregator.AddEvent(protocol.Event{Timestamp: now.Unix(), Type: "speed_test_deferred", Detail: map[string]any{"reason": "endpoint_or_network_failure", "retry_seconds": int(delay.Seconds())}})
}

func (e *speedDeferredError) Error() string { return "speed test deferred: " + e.reason }

func (s *Service) Status(ctx context.Context, includeLive bool) (Status, error) {
	current := s.store.Snapshot()
	effective, _ := effectiveMeasurementConfig(current)
	stats, err := s.spool.Stats(ctx)
	if err != nil {
		return Status{}, err
	}
	s.mu.Lock()
	captive := s.captive
	s.mu.Unlock()
	dailyRemaining, monthlyRemaining := s.store.RemainingDataBudgetsFor(time.Now(), effective.DailyDataBudgetMB, effective.MonthlyDataBudgetMB)
	status := Status{
		Version: serviceVersion(), State: s.lifecycle(time.Now()), Mode: current.Mode, Connected: current.Claimed(), SensorID: current.SensorID, SiteID: current.SiteID,
		ControllerURL: current.ControllerURL, ConfigVersion: current.ConfigVersion,
		SSIDConsent: current.SSIDConsent, SSIDPurgePending: current.SSIDPurgePending, PrivacyPurgeAction: current.PrivacyPurgeAction, SSIDRequested: effective.SendSSID,
		PausedUntil: current.PausedUntil, RevokedAt: current.RevokedAt,
		Spool: stats, Upload: s.uploader.Snapshot(), UptimeSeconds: int64(time.Since(s.started).Seconds()),
		Captive: captive, LastSpeedTest: current.LastSpeedTests[defaultSpeedProfile(effective)], LastSpeedTests: current.LastSpeedTests,
		NextSpeedTests: map[string]time.Time{}, SpeedProfiles: append([]protocol.SpeedProfileConfig(nil), effective.SpeedProfiles...),
		DataBudgetBytes:    dailyRemaining,
		DataBudgetLimit:    uint64(max(effective.DailyDataBudgetMB, 0)) << 20,
		MonthlyBudgetBytes: monthlyRemaining,
		MonthlyBudgetLimit: uint64(max(effective.MonthlyDataBudgetMB, 0)) << 20,
	}
	status.UpdateMethod = s.updateMethod
	status.UpdateChannel, _, status.UpdateChannelLocked = resolveUpdateSource(current.UpdateChannel)
	if s.updateMethod == "apt" {
		status.UpdateChannel = ""
		status.UpdateChannelLocked = true
	}
	status.ManualTestEnvelopes = map[string]uint64{}
	for _, profile := range effective.SpeedProfiles {
		if device, ok := protocol.DeviceSpeedProfile(profile.Name); ok {
			status.ManualTestEnvelopes[profile.Name] = device.MaxDownloadBytes + device.MaxUploadBytes
		}
	}
	_, status.HouseholdAvailable = configuredSpeedProfile(effective, protocol.ProfileHousehold)
	status.HouseholdMix, status.HouseholdMixSource = protocol.EffectiveHouseholdMix(current.HouseholdMix, effective.HouseholdMix)
	primary := defaultSpeedProfile(effective)
	for _, profile := range effective.SpeedProfiles {
		if !profile.Enabled || len(profile.Windows) == 0 || (len(profile.Windows) == 1 && profile.Windows[0] == "never") {
			continue
		}
		next := measurement.NextSpeedProfile(time.Now(), current.LastSpeedTests[profile.Name], profile.Windows, profile.CadenceMinutes, current.SensorUID+":"+profile.Name)
		startupReady := s.started.Add(5 * time.Minute)
		if next.Before(startupReady) {
			next = startupReady
		}
		status.NextSpeedTests[profile.Name] = next
		if profile.Name == primary {
			status.NextSpeedTest = next
		}
	}
	if includeLive {
		status.Live = s.aggregator.Live()
	}
	return status, nil
}

func (s *Service) lifecycle(now time.Time) string {
	current := s.store.Snapshot()
	if current.Mode == state.ModeSetup {
		return "setup"
	}
	if current.Mode == state.ModeConnected && !current.Claimed() {
		return "setup"
	}
	if current.Paused(now) {
		return "paused"
	}
	s.mu.Lock()
	captive := s.captive
	s.mu.Unlock()
	if captive {
		return "captive"
	}
	if current.Mode == state.ModeConnected && s.uploader != nil && s.uploader.Snapshot().Failures > 0 {
		return "offline"
	}
	return "active"
}

func (s *Service) HandleIPC(ctx context.Context, request ipc.Request) (any, *ipc.Error) {
	switch request.Method {
	case "status":
		var params struct {
			Live bool `json:"live"`
		}
		_ = json.Unmarshal(request.Params, &params)
		status, err := s.Status(ctx, params.Live)
		if err != nil {
			return nil, internalError(err)
		}
		return status, nil
	case "watch":
		return s.subscribe(), nil
	case "connect":
		var params struct {
			URL   string `json:"url"`
			Token string `json:"token"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil || params.URL == "" || params.Token == "" {
			return nil, &ipc.Error{Code: "invalid_parameters", Message: "controller URL and claim token are required"}
		}
		if err := protocol.ValidateControllerURL(params.URL); err != nil {
			return nil, &ipc.Error{Code: "invalid_parameters", Message: err.Error()}
		}
		if !s.enrollRun.TryLock() {
			return nil, &ipc.Error{Code: "connect_in_progress", Message: "another enrollment is already in progress"}
		}
		defer s.enrollRun.Unlock()
		if s.store.Snapshot().SSIDPurgePending {
			return nil, &ipc.Error{Code: "privacy_cleanup_pending", Message: "finish the pending privacy cleanup before connecting this sensor"}
		}
		releaseActivity, admitted := s.activity.begin()
		if !admitted {
			return nil, &ipc.Error{Code: "update_in_progress", Message: "finish the software update before connecting this sensor"}
		}
		defer releaseActivity()
		if s.store.Snapshot().Claimed() {
			return nil, &ipc.Error{Code: "already_claimed", Message: "disconnect this sensor before connecting it again"}
		}
		hostname, err := os.Hostname()
		if err != nil || hostname == "" {
			hostname = "axon-pulse"
		}
		enrolled, err := s.client.Enroll(ctx, params.URL, protocol.EnrollmentRequest{
			Token: params.Token, SensorUID: s.store.Snapshot().SensorUID, Hostname: hostname,
			OS: runtime.GOOS, Arch: runtime.GOARCH, AppVersion: serviceVersion(),
		})
		if err != nil {
			var apiErr *protocol.APIError
			if errors.As(err, &apiErr) && apiErr.Code == "invalid_claim_token" {
				return nil, &ipc.Error{Code: "invalid_claim_token", Message: "This invite has expired — ask your provider for a new one."}
			}
			return nil, &ipc.Error{Code: "enrollment_failed", Message: err.Error()}
		}
		// Wait for producers using the previous mode, then end the local-only
		// interval before controller upload can become eligible.
		s.modeRun.Lock()
		if err := s.flushShutdown(ctx, time.Now(), true); err != nil {
			s.modeRun.Unlock()
			return nil, internalError(err)
		}
		if err := s.store.SetEnrollment(strings.TrimRight(params.URL, "/"), enrolled); err != nil {
			s.modeRun.Unlock()
			return nil, internalError(err)
		}
		s.uploader.ClearSpeedTestRequests()
		s.modeRun.Unlock()
		s.uploader.Force()
		if err := s.tickUploadNow(ctx); err != nil && !errors.Is(err, protocol.ErrRevoked) {
			s.logger.Warn("initial_heartbeat_failed", "error", err)
		}
		status, err := s.Status(ctx, false)
		if err != nil {
			return nil, internalError(err)
		}
		s.logger.Info("sensor_enrolled", "sensor_id", enrolled.SensorID, "site_id", enrolled.SiteID)
		return status, nil
	case "pause":
		var params struct {
			Seconds int64 `json:"seconds"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil || params.Seconds < 0 || params.Seconds > int64((7*24*time.Hour)/time.Second) {
			return nil, &ipc.Error{Code: "invalid_parameters", Message: "pause must be between 0 and 604800 seconds"}
		}
		until := time.Time{}
		if params.Seconds > 0 {
			until = time.Now().Add(time.Duration(params.Seconds) * time.Second)
		}
		if err := s.store.SetPausedUntil(until); err != nil {
			return nil, internalError(err)
		}
		status, err := s.Status(ctx, false)
		if err != nil {
			return nil, internalError(err)
		}
		return status, nil
	case "set_mode":
		var params struct {
			Mode string `json:"mode"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil {
			return nil, &ipc.Error{Code: "invalid_parameters", Message: "mode is required"}
		}
		s.modeRun.Lock()
		defer s.modeRun.Unlock()
		current := s.store.Snapshot()
		if params.Mode == current.Mode {
			status, err := s.Status(ctx, false)
			if err != nil {
				return nil, internalError(err)
			}
			return status, nil
		}
		// A mode boundary cannot share an open minute: local samples must never
		// enter a controller row, and partial summaries cannot be merged later.
		if err := s.flushShutdown(ctx, time.Now(), current.Mode == state.ModeStandalone); err != nil {
			return nil, internalError(err)
		}
		if err := s.store.SetMode(params.Mode); err != nil {
			return nil, &ipc.Error{Code: "invalid_mode", Message: err.Error()}
		}
		if params.Mode == state.ModeStandalone {
			s.uploader.ClearSpeedTestRequests()
		}
		status, err := s.Status(ctx, false)
		if err != nil {
			return nil, internalError(err)
		}
		return status, nil
	case "ssid_consent":
		var params struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil {
			return nil, &ipc.Error{Code: "invalid_parameters", Message: "SSID consent must be true or false"}
		}
		s.modeRun.Lock()
		defer s.modeRun.Unlock()
		current := s.store.Snapshot()
		if !current.Claimed() {
			return nil, &ipc.Error{Code: "unclaimed", Message: "connect Axon Pulse before changing SSID consent"}
		}
		if params.Enabled {
			if current.SSIDPurgePending {
				return nil, &ipc.Error{Code: "privacy_cleanup_pending", Message: "finish removing previously collected SSID data before enabling consent"}
			}
			if err := s.store.SetSSIDConsent(true); err != nil {
				return nil, internalError(err)
			}
		} else {
			if err := s.store.BeginSSIDPurge(); err != nil {
				return nil, internalError(err)
			}
			if err := s.completeSSIDPurgeLocked(ctx); err != nil {
				return nil, internalError(err)
			}
		}
		status, err := s.Status(ctx, false)
		if err != nil {
			return nil, internalError(err)
		}
		return status, nil
	case "history":
		var params struct {
			Hours int      `json:"hours"`
			Kinds []string `json:"kinds"`
		}
		_ = json.Unmarshal(request.Params, &params)
		if params.Hours <= 0 || params.Hours > 24*7 {
			params.Hours = 24
		}
		records, err := s.spool.History(ctx, time.Now().Add(-time.Duration(params.Hours)*time.Hour), 10000)
		if err != nil {
			return nil, internalError(err)
		}
		if len(params.Kinds) > 0 {
			keep := make(map[string]bool, len(params.Kinds))
			for _, kind := range params.Kinds {
				keep[kind] = true
			}
			filtered := records[:0]
			for _, record := range records {
				if keep[string(record.Kind)] {
					filtered = append(filtered, record)
				}
			}
			records = filtered
		}
		return boundHistoryPayload(records), nil
	case "support_bundle":
		status, err := s.Status(ctx, false)
		if err != nil {
			return nil, internalError(err)
		}
		records, err := s.spool.History(ctx, time.Now().Add(-24*time.Hour), 10000)
		if err != nil {
			return nil, internalError(err)
		}
		counts := map[string]int{}
		events := map[string]int{}
		errorsByCategory := map[string]int{}
		for _, record := range records {
			counts[string(record.Kind)]++
			var data map[string]any
			if json.Unmarshal(record.Data, &data) != nil {
				continue
			}
			if record.Kind == spool.KindEvent {
				if eventType, ok := data["type"].(string); ok && len(eventType) <= 64 {
					events[eventType]++
				}
			}
			if errorText, ok := data["error"].(string); ok && errorText != "" {
				errorsByCategory[supportErrorCategory(errorText)]++
			}
		}
		upload := status.Upload
		if upload.LastError != "" {
			upload.LastError = supportErrorCategory(upload.LastError)
		}
		s.mu.Lock()
		update := map[string]any{
			"current_version": s.updateStatus.CurrentVersion, "available_version": s.updateStatus.AvailableVersion,
			"staged": s.updateStatus.StagedPath != "", "restarting": s.updateStatus.Restarting, "checked_at": s.updateStatus.CheckedAt,
		}
		s.mu.Unlock()
		effective, _ := effectiveMeasurementConfig(s.store.Snapshot())
		bundle := map[string]any{
			"schema_version": 1, "generated_at": time.Now().UTC(), "app_version": serviceVersion(),
			"platform": map[string]string{"os": runtime.GOOS, "arch": runtime.GOARCH},
			"service":  map[string]any{"state": status.State, "mode": status.Mode, "uptime_seconds": status.UptimeSeconds, "captive": status.Captive},
			"config":   supportConfigSummary(effective), "spool": status.Spool, "upload": upload, "update_status": update,
			"recent_record_counts": counts, "recent_state_transitions": events, "probe_error_categories": errorsByCategory,
		}
		encoded, err := support.Encode(bundle)
		if err != nil {
			return nil, internalError(err)
		}
		var result any
		if err := json.Unmarshal(encoded, &result); err != nil {
			return nil, internalError(err)
		}
		return result, nil
	case "disconnect":
		// Match connect's enrollRun -> modeRun order so a stale enrollment
		// response cannot commit after disconnect has returned.
		s.enrollRun.Lock()
		defer s.enrollRun.Unlock()
		s.modeRun.Lock()
		defer s.modeRun.Unlock()
		if err := s.store.BeginPrivacyPurge(state.PrivacyPurgeDisconnect); err != nil {
			return nil, internalError(err)
		}
		if err := s.completeSSIDPurgeLocked(ctx); err != nil {
			return nil, internalError(err)
		}
		status, err := s.Status(ctx, false)
		if err != nil {
			return nil, internalError(err)
		}
		return status, nil
	case "test":
		current := s.store.Snapshot()
		if !current.Operational() {
			return nil, &ipc.Error{Code: "setup_required", Message: "choose local or connected mode before running a test"}
		}
		var params struct {
			Profile string `json:"profile"`
		}
		_ = json.Unmarshal(request.Params, &params)
		s.mu.Lock()
		if s.speedBusy || s.uploadBusy || s.updateBusy {
			s.mu.Unlock()
			return nil, &ipc.Error{Code: "test_in_progress", Message: "a speed test, upload, or update download is already running"}
		}
		s.speedBusy = true
		s.mu.Unlock()
		defer s.clearBusy("speed")
		result, err := s.runSpeedTest(ctx, params.Profile)
		if err != nil {
			return nil, &ipc.Error{Code: "speed_test_deferred", Message: err.Error()}
		}
		return result, nil
	case "set_household_mix":
		// A null mix clears the local override so controller configuration
		// or the catalog default applies again. The stored mix is normalized
		// so the status always reports exactly what the next test will run.
		var params struct {
			Mix *quality.HouseholdMix `json:"mix"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil {
			return nil, &ipc.Error{Code: "invalid_params", Message: "household mix must be an object or null"}
		}
		if err := s.store.SetHouseholdMix(params.Mix); err != nil {
			return nil, internalError(err)
		}
		status, err := s.Status(ctx, false)
		if err != nil {
			return nil, internalError(err)
		}
		return status, nil
	case "check_update":
		s.updateRun.Lock()
		defer s.updateRun.Unlock()
		status, err := s.checkUpdate(ctx)
		if err != nil && !errors.Is(err, pulseupdate.ErrNoUpdate) {
			return nil, &ipc.Error{Code: "update_check_failed", Message: err.Error()}
		}
		return status, nil
	case "set_update_channel":
		if s.updateMethod == "apt" {
			return nil, &ipc.Error{Code: "update_package_managed", Message: pulseupdate.ErrPackageManaged.Error()}
		}
		var params struct {
			Channel string `json:"channel"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil || !state.ValidUpdateChannel(params.Channel) {
			return nil, &ipc.Error{Code: "invalid_parameters", Message: "channel must be main, beta, or alpha"}
		}
		if _, _, locked := resolveUpdateSource(""); locked {
			return nil, &ipc.Error{Code: "update_channel_locked", Message: "the update source is fixed by AXON_PULSE_UPDATE_INDEX_URL in the service environment"}
		}
		s.updateRun.Lock()
		defer s.updateRun.Unlock()
		s.mu.Lock()
		busy := s.updateBusy || s.updateStatus.Restarting
		s.mu.Unlock()
		if busy {
			return nil, &ipc.Error{Code: "update_in_progress", Message: "an update is downloading or installing; change the update stream when it finishes"}
		}
		if err := s.store.SetUpdateChannel(params.Channel); err != nil {
			return nil, internalError(err)
		}
		_, indexURL, _ := resolveUpdateSource(params.Channel)
		s.updates = s.newUpdateManager(indexURL)
		s.mu.Lock()
		// Anything found or staged from the previous stream no longer applies.
		s.updateStatus.Available = nil
		s.updateStatus.AvailableVersion = ""
		s.updateStatus.StagedPath = ""
		s.updateStatus.CheckedAt = time.Time{}
		s.mu.Unlock()
		status, err := s.checkUpdate(ctx)
		if err != nil && !errors.Is(err, pulseupdate.ErrNoUpdate) {
			status.CheckError = err.Error()
		}
		return status, nil
	case "stage_update":
		if s.updateMethod == "apt" {
			return nil, &ipc.Error{Code: "update_package_managed", Message: pulseupdate.ErrPackageManaged.Error()}
		}
		s.updateRun.Lock()
		defer s.updateRun.Unlock()
		s.mu.Lock()
		// Hold updateBusy for manual downloads too, so a speed test cannot
		// start against update traffic. The automatic path already owns the
		// flag when it reaches here.
		ownBusy := !s.updateBusy
		if ownBusy {
			s.updateBusy = true
		}
		available := s.updateStatus.Available
		s.mu.Unlock()
		if ownBusy {
			defer s.clearBusy("update")
		}
		if available == nil {
			return nil, &ipc.Error{Code: "no_update", Message: "check for an update first"}
		}
		drainCtx, cancelDrain := context.WithTimeout(ctx, s.updateDrainWait)
		resumeActivity, err := s.activity.quiesce(drainCtx)
		cancelDrain()
		if err != nil {
			return nil, &ipc.Error{Code: "update_deferred", Message: err.Error()}
		}
		resumeOnReturn := true
		defer func() {
			if resumeOnReturn {
				resumeActivity()
			}
		}()
		staged, err := s.updates.Stage(ctx, *available, s.stateDir)
		if err != nil {
			return nil, &ipc.Error{Code: "update_stage_failed", Message: err.Error()}
		}
		s.modeRun.Lock()
		defer s.modeRun.Unlock()
		current := s.store.Snapshot()
		if err := s.flushForUpdate(ctx, time.Now(), current.Mode == state.ModeStandalone); err != nil {
			return nil, &ipc.Error{Code: "update_flush_failed", Message: err.Error()}
		}
		executable, executableErr := os.Executable()
		if executableErr != nil {
			return nil, &ipc.Error{Code: "update_activate_failed", Message: executableErr.Error()}
		}
		activateErr := s.activate(ctx, staged, executable, available.Version)
		if activateErr != nil {
			return nil, &ipc.Error{Code: "update_activate_failed", Message: activateErr.Error()}
		}
		resumeOnReturn = false
		restarting := true
		s.mu.Lock()
		s.updateStatus.StagedPath = staged
		s.updateStatus.Restarting = restarting
		status := s.updateStatus
		s.mu.Unlock()
		if status.Restarting {
			s.wg.Go(func() {
				timer := time.NewTimer(s.restartDelay)
				defer timer.Stop()
				<-timer.C
				s.shutdownOnce.Do(func() { close(s.shutdown) })
			})
		}
		return status, nil
	default:
		return nil, &ipc.Error{Code: "unknown_method", Message: fmt.Sprintf("unknown method %q", request.Method)}
	}
}

func configuredSpeedProfile(config protocol.SensorConfig, name string) (protocol.SpeedProfileConfig, bool) {
	for _, profile := range config.SpeedProfiles {
		if profile.Name == name {
			profile.Windows = append([]string(nil), profile.Windows...)
			return profile, true
		}
	}
	return protocol.SpeedProfileConfig{}, false
}

func effectiveMeasurementConfig(current state.State) (protocol.SensorConfig, string) {
	if current.Mode == state.ModeStandalone {
		return protocol.DefaultConfig(), ""
	}
	return current.Config, current.ControllerURL
}

func speedTestBytes(result protocol.SpeedTest) uint64 {
	used := result.Download.BytesTransferred + result.Upload.BytesTransferred
	if total := result.Responsiveness.TotalDownloadBytes + result.Responsiveness.TotalUploadBytes; total > used {
		used = total
	}
	if result.Budget.UsedBytes > used {
		used = result.Budget.UsedBytes
	}
	return used
}

// defaultSpeedProfile is the profile a plain "run a test" means: household
// when the effective configuration offers it, otherwise the legacy content
// profile a contract-2 controller still schedules.
func defaultSpeedProfile(config protocol.SensorConfig) string {
	if _, ok := configuredSpeedProfile(config, protocol.ProfileHousehold); ok {
		return protocol.ProfileHousehold
	}
	return protocol.ProfileContent
}

func experienceEligibility(result protocol.SpeedTest, traffic protocol.TrafficContext, profile protocol.SpeedProfileConfig) (bool, string) {
	if !profile.CountsTowardExperience || !protocol.ExperienceProfile(profile.Name) {
		return false, "profile_excluded"
	}
	if traffic.Contaminated {
		return false, "cross_traffic"
	}
	if profile.Name == protocol.ProfileHousehold {
		return householdEligibility(result)
	}
	if result.Budget.EffectiveDownloadBytes < result.Budget.RequestedDownloadBytes || result.Budget.EffectiveUploadBytes < result.Budget.RequestedUploadBytes {
		return false, "budget_capped"
	}
	if result.Download.Error != "" || result.Upload.Error != "" {
		return false, "required_phase_error"
	}
	if result.Download.BytesTransferred == 0 || result.Download.ThroughputMbps <= 0 || result.Upload.BytesTransferred == 0 || result.Upload.ThroughputMbps <= 0 {
		return false, "throughput_unavailable"
	}
	if result.Responsiveness.Baseline.Count == 0 || result.Responsiveness.DownloadLoaded.Count == 0 || result.Responsiveness.UploadLoaded.Count == 0 {
		return false, "latency_evidence_unavailable"
	}
	if !result.Experience.Available {
		return false, "experience_unavailable"
	}
	if result.MeasurementConfidence.Level == "low" {
		return false, "low_measurement_confidence"
	}
	return true, ""
}

// householdEligibility admits a household result to the representative
// history only when the mix was actually observed for a scorable window.
// An insufficient run is kept as a diagnostic with its stop reason; it never
// replaces a representative result.
func householdEligibility(result protocol.SpeedTest) (bool, string) {
	household := result.Household
	switch {
	case household == nil:
		return false, "household_evidence_missing"
	case household.StopReason != quality.StopEnoughEvidence:
		return false, "household_" + household.StopReason
	case household.Status == quality.HouseholdStatusInsufficient:
		return false, "household_insufficient"
	case result.Responsiveness.Baseline.Count == 0:
		return false, "latency_evidence_unavailable"
	case !result.Experience.Available:
		return false, "experience_unavailable"
	case result.MeasurementConfidence.Level == "low":
		return false, "low_measurement_confidence"
	}
	return true, ""
}

func speedBudgetReason(dailyRemaining, monthlyRemaining uint64) string {
	if min(dailyRemaining, monthlyRemaining) >= 1<<20 {
		return ""
	}
	if monthlyRemaining <= dailyRemaining {
		return "monthly_data_budget"
	}
	return "daily_data_budget"
}

func speedTestDeferral(traffic protocol.TrafficContext, profile protocol.SpeedProfileConfig) string {
	if traffic.Override {
		return ""
	}
	switch {
	case traffic.Contaminated:
		return "cross_traffic"
	case traffic.Metered && !profile.AllowMetered:
		return "metered_connection"
	case traffic.OnBattery && !profile.AllowOnBattery && lowBattery(traffic):
		return "battery_power"
	case protocol.SaturationProfile(profile.Name) && !traffic.MeteredAvailable:
		return "metered_status_unavailable"
	case protocol.SaturationProfile(profile.Name) && !traffic.PowerAvailable:
		return "power_status_unavailable"
	default:
		return ""
	}
}

func lowBattery(traffic protocol.TrafficContext) bool {
	return traffic.BatteryPct <= 0 || traffic.BatteryPct < lowBatteryDeferPct
}

func validateControllerSpeedRequest(request protocol.SpeedTestRequest, now time.Time) string {
	if len(request.Nonce) < 16 || len(request.Nonce) > 128 {
		return "invalid_nonce"
	}
	for _, character := range request.Nonce {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '-' && character != '_' {
			return "invalid_nonce"
		}
	}
	if !protocol.KnownSpeedProfile(request.Profile) {
		return "invalid_profile"
	}
	issued := time.Unix(request.IssuedAt, 0)
	if request.IssuedAt <= 0 || issued.Before(now.Add(-10*time.Minute)) || issued.After(now.Add(30*time.Second)) {
		return "invalid_issued_at"
	}
	expires := time.Unix(request.ExpiresAt, 0)
	if !expires.After(issued) {
		return "invalid_expiry"
	}
	if !expires.After(now) {
		return "expired"
	}
	if expires.After(now.Add(10 * time.Minute)) {
		return "expiry_too_long"
	}
	return ""
}

func applyPseudonymousLinkIdentity(link *protocol.LinkContext, secret string) {
	link.NetworkID = pseudonymousLinkID(secret, "network", link.RawNetworkIdentity)
	link.FirstHopID = pseudonymousLinkID(secret, "first-hop", link.RawFirstHop)
}

func pseudonymousLinkID(secret, domain, value string) string {
	if secret == "" || value == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(domain))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

// boundHistoryPayload keeps the newest records that fit inside a single IPC
// message (capped at 1 MiB). A 24-hour window of records — especially with
// full speed-test evidence attached — can exceed that and would otherwise
// truncate mid-response into a client decode error.
func boundHistoryPayload(records []spool.HistoryRecord) []spool.HistoryRecord {
	const maxPayload = 700 << 10
	total := 0
	start := len(records)
	for index := range slices.Backward(records) {
		total += len(records[index].Data) + 96
		// The newest record is kept even when it alone exceeds the budget;
		// returning nothing would hide the most recent result entirely.
		if total > maxPayload && index != len(records)-1 {
			break
		}
		start = index
	}
	return records[start:]
}

func supportConfigSummary(config protocol.SensorConfig) map[string]any {
	profiles := make([]map[string]any, 0, len(config.SpeedProfiles))
	for _, profile := range config.SpeedProfiles {
		profiles = append(profiles, map[string]any{
			"name": profile.Name, "enabled": profile.Enabled, "window_count": len(profile.Windows),
			"cadence_minutes":      profile.CadenceMinutes,
			"min_duration_seconds": profile.MinDurationSeconds, "max_duration_seconds": profile.MaxDurationSeconds,
			"max_download_bytes": profile.MaxDownloadBytes, "max_upload_bytes": profile.MaxUploadBytes,
			"max_concurrent_requests": profile.MaxConcurrentRequests,
		})
	}
	return map[string]any{
		"contract_version": config.ContractVersion, "probe_interval_seconds": config.ProbeIntervalSeconds,
		"upload_interval_seconds": config.UploadIntervalSeconds, "probe_target_count": len(config.ProbeTargets),
		"dns_target_count": len(config.DNSDomains), "http_target_count": len(config.HTTPTargets),
		"daily_data_budget_mb": config.DailyDataBudgetMB, "monthly_data_budget_mb": config.MonthlyDataBudgetMB,
		"network_identity_enabled": config.SendNetworkIdentity,
		"speed_profiles":           profiles,
	}
}

func supportErrorCategory(value string) string {
	lower := strings.ToLower(value)
	switch {
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline"):
		return "timeout"
	case strings.Contains(lower, "dns") || strings.Contains(lower, "resolve"):
		return "dns"
	case strings.Contains(lower, "tls") || strings.Contains(lower, "certificate"):
		return "tls"
	case strings.Contains(lower, "refused"):
		return "connection_refused"
	case strings.Contains(lower, "network") || strings.Contains(lower, "unreachable"):
		return "network_unreachable"
	default:
		return "other"
	}
}

func internalError(err error) *ipc.Error {
	return &ipc.Error{Code: "internal", Message: err.Error()}
}

func serviceVersion() string {
	if buildinfo.Version == "" {
		return "0.0.0-dev"
	}
	return buildinfo.Version
}

func updateKind() string {
	if buildinfo.Product == "axon-pulse-desktop" {
		return "desktop"
	}
	return "headless"
}

// checkUpdate runs one update check against the current stream. The caller
// holds updateRun.
func (s *Service) checkUpdate(ctx context.Context) (UpdateStatus, error) {
	if s.updateMethod == "apt" {
		return UpdateStatus{CurrentVersion: serviceVersion(), UpdateMethod: "apt", ChannelLocked: true}, nil
	}
	available, err := s.updates.Check(ctx, serviceVersion(), updateKind())
	channel, _, locked := resolveUpdateSource(s.store.Snapshot().UpdateChannel)
	s.mu.Lock()
	s.updateStatus.CheckedAt = time.Now().UTC()
	if err == nil {
		if s.updateStatus.AvailableVersion != available.Version {
			s.updateStatus.StagedPath = ""
			s.updateStatus.Restarting = false
		}
		s.updateStatus.Available = &available
		s.updateStatus.AvailableVersion = available.Version
	} else if errors.Is(err, pulseupdate.ErrNoUpdate) {
		s.updateStatus.Available = nil
		s.updateStatus.AvailableVersion = ""
	}
	status := s.updateStatus
	s.mu.Unlock()
	status.Channel = channel
	status.ChannelLocked = locked
	return status, err
}

// resolveUpdateSource decides which index a service checks. A full index URL
// in the environment pins the source for managed installs and locks the
// stream; otherwise the persisted user choice wins, then the environment's
// channel, then main.
func resolveUpdateSource(persisted string) (channel, indexURL string, locked bool) {
	if configured := strings.TrimSpace(os.Getenv("AXON_PULSE_UPDATE_INDEX_URL")); configured != "" {
		return "", configured, true
	}
	channel = persisted
	if !state.ValidUpdateChannel(channel) {
		channel = strings.TrimSpace(os.Getenv("AXON_PULSE_UPDATE_CHANNEL"))
	}
	if !state.ValidUpdateChannel(channel) {
		channel = state.UpdateChannelMain
	}
	return channel, updateDistributionBase + channel + "/index.json", false
}

func automaticUpdatesEnabled() bool {
	if os.Getenv("AXON_PULSE_DISABLE_AUTO_UPDATE") == "1" {
		return false
	}
	return buildinfo.Version != "" && !strings.Contains(buildinfo.Version, "dev")
}
