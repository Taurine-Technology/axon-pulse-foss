package uploader

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Taurine-Technology/axon-contracts/gen/go/compress"
	"github.com/Taurine-Technology/axon-pulse/internal/protocol"
	"github.com/Taurine-Technology/axon-pulse/internal/spool"
	"github.com/Taurine-Technology/axon-pulse/internal/state"
)

func TestUploaderRetriesIdenticalBatchThenAcknowledges(t *testing.T) {
	t.Parallel()
	var bodies [][]byte
	attempt := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		decoded, _ := compress.Decode(body)
		bodies = append(bodies, decoded)
		attempt++
		if attempt == 1 {
			http.Error(w, `{"code":"ingest_busy"}`, http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(protocol.IngestResponse{ConfigVersion: 1})
	}))
	defer server.Close()
	store, queue := claimedTestState(t, server.URL)
	event := protocol.Event{Timestamp: time.Now().Unix(), Type: "offline_start", Detail: map[string]any{}}
	record, _ := spool.NewRecord(spool.KindEvent, event.Timestamp, event)
	if _, err := queue.AppendMinute(context.Background(), []spool.Record{record}); err != nil {
		t.Fatal(err)
	}
	client := protocol.NewClient()
	client.HTTP = server.Client()
	upload := New(store, queue, client, slog.New(slog.DiscardHandler))
	upload.now = time.Now
	if err := upload.Tick(context.Background()); err == nil {
		t.Fatal("first upload should fail")
	}
	upload.Force()
	if err := upload.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || string(bodies[0]) != string(bodies[1]) {
		t.Fatal("retry body was not stable")
	}
	stats, _ := queue.Stats(context.Background())
	if stats.Records != 0 {
		t.Fatalf("records = %d after ack", stats.Records)
	}
}

func TestUploaderRevocationDurablyBlocksPendingServiceCleanup(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":"sensor_revoked","detail":"revoked"}`)
	}))
	defer server.Close()
	store, queue := claimedTestState(t, server.URL)
	event := protocol.Event{Timestamp: time.Now().Unix(), Type: "offline_start", Detail: map[string]any{}}
	record, _ := spool.NewRecord(spool.KindEvent, event.Timestamp, event)
	_, _ = queue.AppendMinute(context.Background(), []spool.Record{record})
	client := protocol.NewClient()
	client.HTTP = server.Client()
	upload := New(store, queue, client, slog.New(slog.DiscardHandler))
	err := upload.Tick(context.Background())
	if err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("error = %v", err)
	}
	current := store.Snapshot()
	if !current.Claimed() || !current.SSIDPurgePending || current.PrivacyPurgeAction != state.PrivacyPurgeRevocation {
		t.Fatalf("revocation state = %+v", current)
	}
	stats, _ := queue.Stats(context.Background())
	if stats.Records != 1 {
		t.Fatalf("uploader raced service-owned cleanup: %+v", stats)
	}
}

func TestRevocationStateWriteFailureRemainsVolatileFailClosed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not provide a deterministic write fault on Windows")
	}
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":"sensor_revoked","detail":"revoked"}`)
	}))
	defer server.Close()
	store, queue, dir := claimedTestStateInDir(t, server.URL)
	event := protocol.Event{Timestamp: time.Now().Unix(), Type: "private_data", Detail: map[string]any{}}
	record, _ := spool.NewRecord(spool.KindEvent, event.Timestamp, event)
	if _, err := queue.AppendMinute(context.Background(), []spool.Record{record}); err != nil {
		t.Fatal(err)
	}
	client := protocol.NewClient()
	client.HTTP = server.Client()
	upload := New(store, queue, client, slog.New(slog.DiscardHandler))
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o700) }()
	if err := upload.Tick(context.Background()); err == nil {
		t.Fatal("revocation marker write unexpectedly succeeded")
	}
	upload.Force()
	if err := upload.Tick(context.Background()); err == nil {
		t.Fatal("volatile block retry unexpectedly succeeded while state was unwritable")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("network requests after revocation = %d, want 1", got)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	upload.Force()
	if err := upload.Tick(context.Background()); !errors.Is(err, protocol.ErrRevoked) {
		t.Fatalf("durable retry error = %v", err)
	}
	if got := store.Snapshot(); !got.SSIDPurgePending || got.PrivacyPurgeAction != state.PrivacyPurgeRevocation {
		t.Fatalf("durable revocation state = %+v", got)
	}
	reopened, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Snapshot(); !got.SSIDPurgePending || got.PrivacyPurgeAction != state.PrivacyPurgeRevocation {
		t.Fatalf("restart lost durable revocation state = %+v", got)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("network requests after durable retry = %d, want 1", got)
	}
}

func TestUploaderMakesNoNetworkRequestWhileSSIDPurgeIsPending(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	store, queue := claimedTestState(t, server.URL)
	event := protocol.Event{Timestamp: time.Now().Unix(), Type: "contains_prior_consent_data", Detail: map[string]any{}}
	record, err := spool.NewRecord(spool.KindEvent, event.Timestamp, event)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.AppendMinute(context.Background(), []spool.Record{record}); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginSSIDPurge(); err != nil {
		t.Fatal(err)
	}
	client := protocol.NewClient()
	client.HTTP = server.Client()
	upload := New(store, queue, client, slog.New(slog.DiscardHandler))
	if err := upload.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 {
		t.Fatalf("network requests while purge pending = %d", requests.Load())
	}
	stats, err := queue.Stats(context.Background())
	if err != nil || stats.Records != 1 {
		t.Fatalf("fail-closed upload changed queue: stats=%+v err=%v", stats, err)
	}
}

func TestAuthenticatedHeartbeatQueuesBoundedSpeedRequest(t *testing.T) {
	t.Parallel()
	now := time.Now()
	secret := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("s", 32)))
	credentials := protocol.Credentials{SensorID: "sensor", Secret: secret}
	request := protocol.SpeedTestRequest{Nonce: "nonce_1234567890", Profile: "content", IssuedAt: now.Unix(), ExpiresAt: now.Add(2 * time.Minute).Unix()}
	request.Signature, _ = protocol.SignSpeedTestRequest(request, credentials)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != protocol.HeartbeatPath || r.Header.Get("X-Axon-Signature") == "" {
			http.Error(w, "unsigned or unexpected request", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(protocol.HeartbeatResponse{ConfigVersion: 1, SpeedTestRequest: &request})
	}))
	defer server.Close()
	store, queue := claimedTestState(t, server.URL)
	client := protocol.NewClient()
	client.HTTP = server.Client()
	upload := New(store, queue, client, slog.New(slog.DiscardHandler))
	if err := upload.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	pending := upload.PendingSpeedTestRequest()
	if pending == nil || *pending != request {
		t.Fatalf("pending request = %+v", pending)
	}
	upload.ClearSpeedTestRequest(request.Nonce)
	if upload.PendingSpeedTestRequest() != nil {
		t.Fatal("accepted request remained queued")
	}
	upload.queueSpeedTestRequest(&request, credentials)
	upload.ClearSpeedTestRequests()
	if upload.PendingSpeedTestRequest() != nil {
		t.Fatal("trust-boundary clear retained request")
	}
}

func TestHeartbeatRejectsUnsignedSpeedRequest(t *testing.T) {
	t.Parallel()
	now := time.Now()
	request := protocol.SpeedTestRequest{Nonce: "nonce_1234567890", Profile: "content", IssuedAt: now.Unix(), ExpiresAt: now.Add(2 * time.Minute).Unix()}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(protocol.HeartbeatResponse{ConfigVersion: 1, SpeedTestRequest: &request})
	}))
	defer server.Close()
	store, queue := claimedTestState(t, server.URL)
	client := protocol.NewClient()
	client.HTTP = server.Client()
	upload := New(store, queue, client, slog.New(slog.DiscardHandler))
	if err := upload.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if upload.PendingSpeedTestRequest() != nil {
		t.Fatal("unsigned controller request was queued")
	}
}

func claimedTestState(t *testing.T, serverURL string) (*state.Store, *spool.Spool) {
	t.Helper()
	store, queue, _ := claimedTestStateInDir(t, serverURL)
	return store, queue
}

func claimedTestStateInDir(t *testing.T, serverURL string) (*state.Store, *spool.Spool, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	secret := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("s", 32)))
	if err := store.SetEnrollment(serverURL, protocol.EnrollmentResponse{
		SensorID: "sensor", SensorSecret: secret, SiteID: "site", IngestURL: serverURL + protocol.IngestPath,
		Config: protocol.DefaultConfig(), ConfigVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	queue, err := spool.Open(dir+"/spool.db", spool.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = queue.Close() })
	return store, queue, dir
}

func TestQuotaRejectionBacksOffOnQuotaScaleAndHonorsRetryAfter(t *testing.T) {
	t.Parallel()
	base := time.Unix(1_800_000_000, 0)
	fresh := func() *Uploader {
		return &Uploader{log: slog.New(slog.DiscardHandler), now: func() time.Time { return base }}
	}
	quota := fresh()
	quota.fail(&protocol.APIError{StatusCode: http.StatusTooManyRequests, Code: "daily_record_quota"}, 0)
	wait := quota.Snapshot().NextAttempt.Sub(base)
	if wait < 30*time.Minute || wait > time.Hour {
		t.Fatalf("quota 429 retry in %v, want 30-60 minutes", wait)
	}
	honored := fresh()
	honored.fail(&protocol.APIError{StatusCode: http.StatusTooManyRequests, RetryAfter: 2 * time.Hour}, 2*time.Hour)
	if got := honored.Snapshot().NextAttempt.Sub(base); got != 2*time.Hour {
		t.Fatalf("explicit Retry-After scheduled %v, want 2h", got)
	}
	capped := fresh()
	capped.fail(&protocol.APIError{StatusCode: http.StatusTooManyRequests, RetryAfter: 48 * time.Hour}, 48*time.Hour)
	if got := capped.Snapshot().NextAttempt.Sub(base); got != maximumRetryAfter {
		t.Fatalf("oversized Retry-After scheduled %v, want cap %v", got, maximumRetryAfter)
	}
	transport := fresh()
	transport.fail(errors.New("dial tcp: connection refused"), 0)
	if got := transport.Snapshot().NextAttempt.Sub(base); got > maximumBackoff {
		t.Fatalf("transport failure retry %v escaped the short ceiling %v", got, maximumBackoff)
	}
	escalated := fresh()
	for range 4 {
		escalated.fail(&protocol.APIError{StatusCode: http.StatusTooManyRequests, Code: "daily_record_quota"}, 0)
	}
	if got := escalated.Snapshot().NextAttempt.Sub(base); got < 4*time.Hour || got > maximumRetryAfter {
		t.Fatalf("fourth quota 429 retry in %v, want escalation into 4h-%v", got, maximumRetryAfter)
	}
	flat := fresh()
	for range 4 {
		flat.fail(&protocol.APIError{StatusCode: http.StatusTooManyRequests}, 0)
	}
	if got := flat.Snapshot().NextAttempt.Sub(base); got < 30*time.Minute || got > time.Hour {
		t.Fatalf("non-quota 429 retry in %v, want flat 30-60 minutes", got)
	}
}
