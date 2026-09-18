package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Taurine-Technology/axon-contracts/gen/go/buildinfo"
	"github.com/Taurine-Technology/axon-pulse/internal/aggregate"
	"github.com/Taurine-Technology/axon-pulse/internal/ipc"
	"github.com/Taurine-Technology/axon-pulse/internal/measurement"
	"github.com/Taurine-Technology/axon-pulse/internal/protocol"
	"github.com/Taurine-Technology/axon-pulse/internal/spool"
	"github.com/Taurine-Technology/axon-pulse/internal/state"
	pulseupdate "github.com/Taurine-Technology/axon-pulse/internal/update"
	"github.com/Taurine-Technology/axon-pulse/quality"
)

type (
	emptyProbe struct{}

	blockingProbe struct {
		started  chan string
		release  chan struct{}
		sampleAt time.Time
	}

	blockingOperationProbe struct {
		operation string
		once      sync.Once
		started   chan struct{}
		release   chan struct{}
	}

	cancellationProbe struct {
		started chan struct{}
		done    chan struct{}
	}

	blockingCollector struct {
		once    sync.Once
		started chan struct{}
		release chan struct{}
	}

	staticCollector struct{}

	fakeSpeedRunner struct {
		result protocol.SpeedTest
		err    error
	}

	blockingSpeedRunner struct {
		started chan struct{}
		release chan struct{}
	}

	recordingSpeedRunner struct {
		traffic protocol.TrafficContext
		profile protocol.SpeedProfileConfig
		result  protocol.SpeedTest
	}

	fakeUpdateManager struct {
		stageCalled chan struct{}
		stagePath   string
		stageErr    error
	}

	errorCloser struct{ err error }
)

func (c errorCloser) Close() error { return c.err }

func (f *fakeUpdateManager) Check(context.Context, string, string) (pulseupdate.Available, error) {
	return pulseupdate.Available{}, pulseupdate.ErrNoUpdate
}

func (f *fakeUpdateManager) Stage(context.Context, pulseupdate.Available, string) (string, error) {
	if f.stageCalled != nil {
		f.stageCalled <- struct{}{}
	}
	return f.stagePath, f.stageErr
}

func TestConnectEnrollsAndSendsInitialHeartbeat(t *testing.T) {
	t.Parallel()
	secret := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
	var heartbeat atomic.Bool
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case protocol.EnrollPath:
			var enrollment protocol.EnrollmentRequest
			if err := json.NewDecoder(request.Body).Decode(&enrollment); err != nil {
				t.Errorf("decode enrollment: %v", err)
			}
			if enrollment.Token != "spt_test_token_123456" || enrollment.SensorUID == "" {
				t.Errorf("bad enrollment: %+v", enrollment)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(protocol.EnrollmentResponse{
				SensorID: "018f0000-0000-7000-8000-000000000001", SensorSecret: secret,
				SiteID: "site", Config: protocol.DefaultConfig(), ConfigVersion: 1,
				IngestURL: server.URL + protocol.IngestPath,
			})
		case protocol.HeartbeatPath:
			if request.Header.Get("X-Axon-Sensor-Proto") != "1" || request.Header.Get("X-Axon-Signature") == "" {
				t.Error("heartbeat was not HMAC authenticated")
			}
			heartbeat.Store(true)
			_ = json.NewEncoder(w).Encode(protocol.HeartbeatResponse{ConfigVersion: 1})
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	daemon, err := Open(dir, filepath.Join(dir, "pulse.sock"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Close() }()
	daemon.client.HTTP = server.Client()
	params, _ := json.Marshal(map[string]string{"url": server.URL, "token": "spt_test_token_123456"})
	result, ipcErr := daemon.HandleIPC(context.Background(), ipc.Request{Method: "connect", Params: params})
	if ipcErr != nil {
		t.Fatal(ipcErr)
	}
	status, ok := result.(Status)
	if !ok || status.State != "active" || status.SensorID == "" {
		t.Fatalf("unexpected status: %#v", result)
	}
	if !heartbeat.Load() {
		t.Fatal("initial heartbeat was not sent")
	}
}

func TestConnectRejectsPlaintextControllerURL(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	daemon, err := Open(dir, filepath.Join(dir, "pulse.sock"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Close() }()
	params, _ := json.Marshal(map[string]string{"url": "http://localhost:8080", "token": "spt_test_token_123456"})
	_, ipcErr := daemon.HandleIPC(context.Background(), ipc.Request{Method: "connect", Params: params})
	if ipcErr == nil || ipcErr.Code != "invalid_parameters" {
		t.Fatalf("connect error = %+v", ipcErr)
	}
}

func TestConcurrentConnectAllowsOnlyOneEnrollmentRequest(t *testing.T) {
	t.Parallel()
	secret := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
	started := make(chan struct{})
	release := make(chan struct{})
	var requests atomic.Int32
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case protocol.EnrollPath:
			if requests.Add(1) == 1 {
				close(started)
			}
			<-release
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(protocol.EnrollmentResponse{
				SensorID: "sensor", SensorSecret: secret, SiteID: "site",
				IngestURL: server.URL + protocol.IngestPath, Config: protocol.DefaultConfig(), ConfigVersion: 1,
			})
		case protocol.HeartbeatPath:
			_ = json.NewEncoder(writer).Encode(protocol.HeartbeatResponse{ConfigVersion: 1})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	daemon, err := Open(dir, filepath.Join(dir, "pulse.sock"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Close() }()
	daemon.client.HTTP = server.Client()
	params, _ := json.Marshal(map[string]string{"url": server.URL, "token": "spt_test_token_123456"})
	firstDone := make(chan *ipc.Error, 1)
	go func() {
		_, ipcErr := daemon.HandleIPC(context.Background(), ipc.Request{Method: "connect", Params: params})
		firstDone <- ipcErr
	}()
	<-started
	_, secondErr := daemon.HandleIPC(context.Background(), ipc.Request{Method: "connect", Params: params})
	if secondErr == nil || secondErr.Code != "connect_in_progress" {
		t.Fatalf("second connect error = %+v", secondErr)
	}
	close(release)
	if ipcErr := <-firstDone; ipcErr != nil {
		t.Fatal(ipcErr)
	}
	if requests.Load() != 1 || !daemon.store.Snapshot().Claimed() {
		t.Fatalf("enrollment requests=%d state=%+v", requests.Load(), daemon.store.Snapshot())
	}
}

func TestDisconnectCannotBeOvertakenByInflightEnrollment(t *testing.T) {
	t.Parallel()
	secret := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("d", 32)))
	started, release := make(chan struct{}), make(chan struct{})
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case protocol.EnrollPath:
			close(started)
			<-release
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(protocol.EnrollmentResponse{
				SensorID: "sensor", SensorSecret: secret, SiteID: "site",
				IngestURL: server.URL + protocol.IngestPath, Config: protocol.DefaultConfig(), ConfigVersion: 1,
			})
		case protocol.HeartbeatPath:
			_ = json.NewEncoder(writer).Encode(protocol.HeartbeatResponse{ConfigVersion: 1})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	daemon, err := Open(dir, filepath.Join(dir, "pulse.sock"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Close() }()
	daemon.client.HTTP = server.Client()
	params, _ := json.Marshal(map[string]string{"url": server.URL, "token": "spt_test_token_123456"})
	connectDone := make(chan *ipc.Error, 1)
	go func() {
		_, ipcErr := daemon.HandleIPC(context.Background(), ipc.Request{Method: "connect", Params: params})
		connectDone <- ipcErr
	}()
	<-started
	disconnectDone := make(chan *ipc.Error, 1)
	go func() {
		_, ipcErr := daemon.HandleIPC(context.Background(), ipc.Request{Method: "disconnect"})
		disconnectDone <- ipcErr
	}()
	select {
	case <-disconnectDone:
		t.Fatal("disconnect crossed an in-flight enrollment")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if ipcErr := <-connectDone; ipcErr != nil {
		t.Fatal(ipcErr)
	}
	if ipcErr := <-disconnectDone; ipcErr != nil {
		t.Fatal(ipcErr)
	}
	if final := daemon.store.Snapshot(); final.Claimed() || final.SSIDPurgePending {
		t.Fatalf("stale enrollment survived disconnect: %+v", final)
	}
}

func TestFailedConnectReleasesEnrollmentAdmission(t *testing.T) {
	t.Parallel()
	secret := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
	var attempts atomic.Int32
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case protocol.EnrollPath:
			if attempts.Add(1) == 1 {
				http.Error(writer, `{"code":"temporary"}`, http.StatusServiceUnavailable)
				return
			}
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(protocol.EnrollmentResponse{
				SensorID: "sensor", SensorSecret: secret, SiteID: "site",
				IngestURL: server.URL + protocol.IngestPath, Config: protocol.DefaultConfig(), ConfigVersion: 1,
			})
		case protocol.HeartbeatPath:
			_ = json.NewEncoder(writer).Encode(protocol.HeartbeatResponse{ConfigVersion: 1})
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	daemon, err := Open(dir, filepath.Join(dir, "pulse.sock"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Close() }()
	daemon.client.HTTP = server.Client()
	params, _ := json.Marshal(map[string]string{"url": server.URL, "token": "spt_test_token_123456"})
	if _, ipcErr := daemon.HandleIPC(context.Background(), ipc.Request{Method: "connect", Params: params}); ipcErr == nil {
		t.Fatal("first enrollment unexpectedly succeeded")
	}
	if _, ipcErr := daemon.HandleIPC(context.Background(), ipc.Request{Method: "connect", Params: params}); ipcErr != nil {
		t.Fatalf("second enrollment failed after admission should have reopened: %v", ipcErr)
	}
	if attempts.Load() != 2 {
		t.Fatalf("enrollment attempts = %d, want 2", attempts.Load())
	}
}

func TestSSIDConsentRevocationClearsLocalTelemetry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	daemon, err := Open(dir, filepath.Join(dir, "pulse.sock"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Close() }()
	if err := daemon.store.SetEnrollment("https://controller.example", protocol.EnrollmentResponse{
		SensorID: "sensor", SensorSecret: "secret", SiteID: "site", IngestURL: "https://controller.example/ingest",
		Config: protocol.SensorConfig{SendSSID: true}, ConfigVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	enabled, _ := json.Marshal(map[string]bool{"enabled": true})
	result, ipcErr := daemon.HandleIPC(context.Background(), ipc.Request{Method: "ssid_consent", Params: enabled})
	enabledStatus, ok := result.(Status)
	if ipcErr != nil || !ok || !enabledStatus.SSIDConsent {
		t.Fatalf("enable result = %#v, error = %v", result, ipcErr)
	}
	link := protocol.LinkContext{Timestamp: 1_800_000_000, InterfaceType: "wifi", SSID: "Private WiFi"}
	record, _ := spool.NewRecord(spool.KindLinkContext, link.Timestamp, link)
	if _, err := daemon.spool.AppendMinute(context.Background(), []spool.Record{record}); err != nil {
		t.Fatal(err)
	}
	disabled, _ := json.Marshal(map[string]bool{"enabled": false})
	result, ipcErr = daemon.HandleIPC(context.Background(), ipc.Request{Method: "ssid_consent", Params: disabled})
	disabledStatus, ok := result.(Status)
	if ipcErr != nil || !ok || disabledStatus.SSIDConsent {
		t.Fatalf("disable result = %#v, error = %v", result, ipcErr)
	}
	stats, err := daemon.spool.Stats(context.Background())
	if err != nil || stats.Records != 0 {
		t.Fatalf("spool after withdrawal = %+v, error = %v", stats, err)
	}
}

func TestSSIDWithdrawalFailureRemainsFailClosedAcrossRestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	socket := shortSocketPath(t, "pulse-service-")
	daemon, err := Open(dir, socket, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err := daemon.store.SetEnrollment("https://controller.example", protocol.EnrollmentResponse{
		SensorID: "sensor", SensorSecret: "secret", SiteID: "site",
		IngestURL: "https://controller.example/ingest", Config: protocol.SensorConfig{SendSSID: true}, ConfigVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := daemon.store.SetSSIDConsent(true); err != nil {
		t.Fatal(err)
	}
	link := protocol.LinkContext{Timestamp: time.Now().Unix(), InterfaceType: "wifi", SSID: "Private WiFi"}
	record, err := spool.NewRecord(spool.KindLinkContext, link.Timestamp, link)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := daemon.spool.AppendMinute(context.Background(), []spool.Record{record}); err != nil {
		t.Fatal(err)
	}
	clearFailure := errors.New("injected clear failure")
	daemon.clearSpool = func(context.Context) error { return clearFailure }
	disabled, _ := json.Marshal(map[string]bool{"enabled": false})
	if _, ipcErr := daemon.HandleIPC(context.Background(), ipc.Request{Method: "ssid_consent", Params: disabled}); ipcErr == nil {
		t.Fatal("SSID withdrawal unexpectedly succeeded")
	}
	if got := daemon.store.Snapshot(); got.SSIDConsent || !got.SSIDPurgePending {
		t.Fatalf("failed withdrawal state = %+v", got)
	}
	if err := daemon.uploader.Tick(context.Background()); err != nil {
		t.Fatalf("fail-closed uploader returned %v", err)
	}
	if err := daemon.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir, socket, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if !reopened.store.Snapshot().SSIDPurgePending {
		t.Fatal("restart forgot pending privacy cleanup")
	}
	stats, err := reopened.spool.Stats(context.Background())
	if err != nil || stats.Records == 0 {
		t.Fatalf("test did not preserve queued data across injected failure: stats=%+v err=%v", stats, err)
	}
	if err := reopened.completeSSIDPurge(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats, err = reopened.spool.Stats(context.Background())
	if err != nil || stats.Records != 0 || reopened.store.Snapshot().SSIDPurgePending {
		t.Fatalf("retried privacy cleanup = stats=%+v state=%+v err=%v", stats, reopened.store.Snapshot(), err)
	}
}

func TestDisconnectCleanupIntentSurvivesFailureAndRestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	socket := filepath.Join(dir, "pulse.sock")
	daemon, err := Open(dir, socket, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err := daemon.store.SetEnrollment("https://controller.example", protocol.EnrollmentResponse{
		SensorID: "sensor", SensorSecret: "secret", SiteID: "site", IngestURL: "https://controller.example/ingest",
		Config: protocol.DefaultConfig(), ConfigVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	daemon.clearSpool = func(context.Context) error { return errors.New("injected clear failure") }
	if _, ipcErr := daemon.HandleIPC(context.Background(), ipc.Request{Method: "disconnect"}); ipcErr == nil {
		t.Fatal("disconnect unexpectedly succeeded")
	}
	failed := daemon.store.Snapshot()
	if !failed.Claimed() || !failed.SSIDPurgePending || failed.PrivacyPurgeAction != state.PrivacyPurgeDisconnect {
		t.Fatalf("failed disconnect state = %+v", failed)
	}
	if err := daemon.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir, socket, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if err := reopened.completeSSIDPurge(context.Background()); err != nil {
		t.Fatal(err)
	}
	completed := reopened.store.Snapshot()
	if completed.Claimed() || completed.SSIDPurgePending || completed.PrivacyPurgeAction != "" {
		t.Fatalf("recovered disconnect state = %+v", completed)
	}
}

func TestRevocationWaitsForConsentedCollectorBeforePrivacyCleanup(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":"sensor_revoked","detail":"revoked"}`)
	}))
	defer server.Close()
	dir := t.TempDir()
	daemon, err := Open(dir, filepath.Join(dir, "pulse.sock"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Close() }()
	config := protocol.DefaultConfig()
	config.SendSSID = true
	secret := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("s", 32)))
	if err := daemon.store.SetEnrollment(server.URL, protocol.EnrollmentResponse{
		SensorID: "sensor", SensorSecret: secret, SiteID: "site", IngestURL: server.URL + protocol.IngestPath,
		Config: config, ConfigVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := daemon.store.SetSSIDConsent(true); err != nil {
		t.Fatal(err)
	}
	record, err := spool.NewRecord(spool.KindEvent, time.Now().Unix(), protocol.Event{Timestamp: time.Now().Unix(), Type: "queued_private_data"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := daemon.spool.AppendMinute(context.Background(), []spool.Record{record}); err != nil {
		t.Fatal(err)
	}
	daemon.client.HTTP = server.Client()
	collector := &blockingCollector{started: make(chan struct{}), release: make(chan struct{})}
	daemon.collector = collector
	daemon.prober = emptyProbe{}
	daemon.mu.Lock()
	daemon.probeBusy, daemon.dnsBusy = true, true
	daemon.mu.Unlock()
	daemon.tick(context.Background(), time.Now())
	<-collector.started

	deadline := time.Now().Add(2 * time.Second)
	for !daemon.store.Snapshot().SSIDPurgePending && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	pending := daemon.store.Snapshot()
	if pending.PrivacyPurgeAction != state.PrivacyPurgeRevocation {
		t.Fatalf("revocation marker = %+v", pending)
	}
	cleanupDone := make(chan error, 1)
	go func() { cleanupDone <- daemon.completeSSIDPurge(context.Background()) }()
	select {
	case err := <-cleanupDone:
		t.Fatalf("cleanup overtook consented collector: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(collector.release)
	if err := <-cleanupDone; err != nil {
		t.Fatal(err)
	}
	completed := daemon.store.Snapshot()
	if completed.Claimed() || completed.SSIDPurgePending || completed.RevokedAt.IsZero() {
		t.Fatalf("completed revocation state = %+v", completed)
	}
	if live := daemon.aggregator.Live(); len(live) != 0 {
		t.Fatalf("revocation left collector output live: %+v", live)
	}
	stats, err := daemon.spool.Stats(context.Background())
	if err != nil || stats.Records != 0 {
		t.Fatalf("revocation left queued telemetry: %+v err=%v", stats, err)
	}
}

func TestForcedFlushKeepsCurrentMinuteMetricOpen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	daemon, err := Open(dir, filepath.Join(dir, "pulse.sock"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Close() }()
	minute := time.Now().UTC().Truncate(time.Minute)
	daemon.aggregator.AddSample(aggregate.Sample{Timestamp: minute.Add(5 * time.Second), Target: "anchor", RTTMS: 10, Success: true})
	if err := daemon.aggregator.AddSpeedTest(protocol.SpeedTest{Timestamp: minute.Add(20 * time.Second).Unix()}); err != nil {
		t.Fatal(err)
	}

	daemon.flush(context.Background(), minute.Add(30*time.Second), false)
	history, err := daemon.spool.History(context.Background(), minute.Add(-time.Second), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].Kind != spool.KindSpeedTest {
		t.Fatalf("mid-minute history = %+v, want only speed-test record", history)
	}

	daemon.aggregator.AddSample(aggregate.Sample{Timestamp: minute.Add(40 * time.Second), Target: "anchor", RTTMS: 20, Success: true})
	daemon.flush(context.Background(), minute.Add(time.Minute), false)
	history, err = daemon.spool.History(context.Background(), minute.Add(-time.Second), 100)
	if err != nil {
		t.Fatal(err)
	}
	minuteMetrics := 0
	for _, record := range history {
		if record.Kind != spool.KindMinuteMetric {
			continue
		}
		minuteMetrics++
		var metric protocol.MinuteMetric
		if err := json.Unmarshal(record.Data, &metric); err != nil {
			t.Fatal(err)
		}
		if metric.Timestamp != minute.Unix() || metric.Samples != 2 {
			t.Fatalf("minute metric = %+v, want complete two-sample minute", metric)
		}
	}
	if minuteMetrics != 1 {
		t.Fatalf("minute metrics = %d, want exactly 1; history = %+v", minuteMetrics, history)
	}

	daemon.flush(context.Background(), minute.Add(2*time.Minute), false)
	history, err = daemon.spool.History(context.Background(), minute.Add(-time.Second), 100)
	if err != nil {
		t.Fatal(err)
	}
	minuteMetrics = 0
	for _, record := range history {
		if record.Kind == spool.KindMinuteMetric {
			minuteMetrics++
		}
	}
	if minuteMetrics != 1 {
		t.Fatalf("repeated flush produced %d minute metrics, want exactly 1", minuteMetrics)
	}
}

func TestFlushWaitsForLatencyProducerBeforeFinalizingMinute(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	daemon, err := Open(dir, filepath.Join(dir, "pulse.sock"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Close() }()
	minute := time.Now().UTC().Truncate(time.Minute).Add(-time.Minute)
	daemon.aggregator.AddSample(aggregate.Sample{Timestamp: minute.Add(5 * time.Second), Target: "anchor", RTTMS: 10, Success: true})
	if err := daemon.aggregator.AddSpeedTest(protocol.SpeedTest{Timestamp: minute.Add(20 * time.Second).Unix()}); err != nil {
		t.Fatal(err)
	}
	daemon.mu.Lock()
	daemon.probeBusy = true
	daemon.mu.Unlock()

	if finalized := daemon.flush(context.Background(), minute.Add(time.Minute), false); finalized {
		t.Fatal("flush finalized a minute while a latency producer was active")
	}
	history, err := daemon.spool.History(context.Background(), minute.Add(-time.Second), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].Kind != spool.KindSpeedTest {
		t.Fatalf("busy flush history = %+v, want only pending speed-test record", history)
	}

	// Simulate the active batch publishing a sample timestamped in the minute
	// that would otherwise already have been finalized.
	daemon.aggregator.AddSample(aggregate.Sample{Timestamp: minute.Add(40 * time.Second), Target: "anchor", RTTMS: 20, Success: true})
	daemon.clearBusy("probe")
	if finalized := daemon.flush(context.Background(), minute.Add(time.Minute), false); !finalized {
		t.Fatal("flush did not finalize after the latency producer became idle")
	}
	history, err = daemon.spool.History(context.Background(), minute.Add(-time.Second), 100)
	if err != nil {
		t.Fatal(err)
	}
	minuteMetrics := 0
	for _, record := range history {
		if record.Kind != spool.KindMinuteMetric {
			continue
		}
		minuteMetrics++
		var metric protocol.MinuteMetric
		if err := json.Unmarshal(record.Data, &metric); err != nil {
			t.Fatal(err)
		}
		if metric.Timestamp != minute.Unix() || metric.Samples != 2 {
			t.Fatalf("minute metric = %+v, want both pre-boundary and late-published samples", metric)
		}
	}
	if minuteMetrics != 1 {
		t.Fatalf("minute metrics = %d, want exactly 1; history = %+v", minuteMetrics, history)
	}
}

func TestModeBoundaryWaitsForProducerAndKeepsLocalRecordsOutOfUpload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	daemon, err := Open(dir, filepath.Join(dir, "pulse.sock"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Close() }()
	if err := daemon.store.SetEnrollment("https://controller.example", protocol.EnrollmentResponse{
		SensorID: "sensor", SensorSecret: "secret", SiteID: "site", IngestURL: "https://controller.example/ingest",
		Config: protocol.DefaultConfig(), ConfigVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := daemon.store.SetMode("standalone"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	_ = daemon.aggregator.AddEvent(protocol.Event{Timestamp: now.Unix(), Type: "local_before"})

	// This read lock represents a measurement producer. The mode writer must
	// wait until that producer has published its final local record.
	daemon.modeRun.RLock()
	params, _ := json.Marshal(map[string]string{"mode": "connected"})
	done := make(chan *ipc.Error, 1)
	go func() {
		_, ipcErr := daemon.HandleIPC(context.Background(), ipc.Request{Method: "set_mode", Params: params})
		done <- ipcErr
	}()
	_ = daemon.aggregator.AddEvent(protocol.Event{Timestamp: now.Unix(), Type: "local_in_flight"})
	select {
	case <-done:
		t.Fatal("mode changed before the active producer completed")
	case <-time.After(20 * time.Millisecond):
	}
	daemon.modeRun.RUnlock()
	if ipcErr := <-done; ipcErr != nil {
		t.Fatal(ipcErr)
	}

	_ = daemon.aggregator.AddEvent(protocol.Event{Timestamp: now.Unix(), Type: "connected_after"})
	if err := daemon.flushShutdown(context.Background(), now.Add(time.Second), false); err != nil {
		t.Fatal(err)
	}
	batch, err := daemon.spool.NextBatch(context.Background(), "test", 1)
	if err != nil {
		t.Fatal(err)
	}
	if batch == nil || len(batch.Batch.Events) != 1 || batch.Batch.Events[0].Type != "connected_after" {
		t.Fatalf("controller batch crossed mode boundary: %+v", batch)
	}
}

func TestTickSnapshotCannotLaunchOldModeProducerAcrossBoundary(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	daemon, err := Open(dir, filepath.Join(dir, "pulse.sock"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Close() }()
	if err := daemon.store.SetEnrollment("https://controller.example", protocol.EnrollmentResponse{
		SensorID: "sensor", SensorSecret: "secret", SiteID: "site", IngestURL: "https://controller.example/ingest",
		Config: protocol.DefaultConfig(), ConfigVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := daemon.store.SetMode("standalone"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	probe := &blockingProbe{started: make(chan string, 1), release: make(chan struct{}), sampleAt: now.Add(-time.Minute)}
	daemon.prober = probe
	daemon.mu.Lock()
	daemon.dnsBusy, daemon.httpBusy, daemon.uploadBusy = true, true, true
	daemon.mu.Unlock()
	daemon.tick(context.Background(), now)
	if controllerURL := <-probe.started; controllerURL != "" {
		t.Fatalf("standalone producer received retained controller URL %q", controllerURL)
	}
	params, _ := json.Marshal(map[string]string{"mode": "connected"})
	done := make(chan *ipc.Error, 1)
	go func() {
		_, ipcErr := daemon.HandleIPC(context.Background(), ipc.Request{Method: "set_mode", Params: params})
		done <- ipcErr
	}()
	select {
	case <-done:
		t.Fatal("mode boundary overtook a producer launched from the old snapshot")
	case <-time.After(20 * time.Millisecond):
	}
	close(probe.release)
	if ipcErr := <-done; ipcErr != nil {
		t.Fatal(ipcErr)
	}
	batch, err := daemon.spool.NextBatch(context.Background(), "test", 1)
	if err != nil {
		t.Fatal(err)
	}
	if batch != nil {
		t.Fatalf("old-mode measurement became upload eligible: %+v", batch)
	}
}

func TestSSIDWithdrawalWaitsForCollectorThenClearsItsResult(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	daemon, err := Open(dir, filepath.Join(dir, "pulse.sock"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Close() }()
	config := protocol.DefaultConfig()
	config.SendSSID = true
	if err := daemon.store.SetEnrollment("https://controller.example", protocol.EnrollmentResponse{
		SensorID: "sensor", SensorSecret: "secret", SiteID: "site", IngestURL: "https://controller.example/ingest", Config: config, ConfigVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := daemon.store.SetSSIDConsent(true); err != nil {
		t.Fatal(err)
	}
	collector := &blockingCollector{started: make(chan struct{}), release: make(chan struct{})}
	daemon.collector = collector
	daemon.prober = emptyProbe{}
	daemon.mu.Lock()
	daemon.probeBusy, daemon.dnsBusy, daemon.uploadBusy = true, true, true
	daemon.mu.Unlock()
	daemon.tick(context.Background(), time.Now())
	<-collector.started
	disabled, _ := json.Marshal(map[string]bool{"enabled": false})
	done := make(chan *ipc.Error, 1)
	go func() {
		_, ipcErr := daemon.HandleIPC(context.Background(), ipc.Request{Method: "ssid_consent", Params: disabled})
		done <- ipcErr
	}()
	select {
	case <-done:
		t.Fatal("SSID withdrawal completed while the consented collector was active")
	case <-time.After(20 * time.Millisecond):
	}
	close(collector.release)
	if ipcErr := <-done; ipcErr != nil {
		t.Fatal(ipcErr)
	}
	if live := daemon.aggregator.Live(); len(live) != 0 {
		t.Fatalf("withdrawal left live telemetry: %+v", live)
	}
	stats, err := daemon.spool.Stats(context.Background())
	if err != nil || stats.Records != 0 {
		t.Fatalf("withdrawal left queued telemetry: %+v err=%v", stats, err)
	}
}

func TestAbortedSpeedTestChargesPartialUseAgainstDurableBudget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	daemon, err := Open(dir, filepath.Join(dir, "pulse.sock"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Close() }()
	if err := daemon.store.SetMode("standalone"); err != nil {
		t.Fatal(err)
	}
	daemon.collector = staticCollector{}
	const used = uint64(2 << 20)
	daemon.speed = fakeSpeedRunner{result: protocol.SpeedTest{Budget: quality.Budget{UsedBytes: used}}, err: context.Canceled}
	now := time.Now()
	before := daemon.store.RemainingDataBudget(now)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := daemon.runSpeedTest(ctx, "household"); !errors.Is(err, context.Canceled) {
		t.Fatalf("aborted test error=%v", err)
	}
	if after := daemon.store.RemainingDataBudget(now); after != before-used {
		t.Fatalf("aborted bytes were not charged: before=%d after=%d", before, after)
	}
}

func TestOverrideRetainsCrossTrafficContamination(t *testing.T) {
	t.Parallel()
	traffic := protocol.TrafficContext{CrossTrafficMbps: 25, Contaminated: true, Override: true}
	if reason := speedTestDeferral(traffic, protocol.SpeedProfileConfig{}); reason != "" {
		t.Fatalf("manual override was deferred: %s", reason)
	}
	dir := t.TempDir()
	daemon, err := Open(dir, filepath.Join(dir, "pulse.sock"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Close() }()
	if err := daemon.store.SetMode("standalone"); err != nil {
		t.Fatal(err)
	}
	daemon.collector = staticCollector{}
	daemon.crossTraffic = func(context.Context, time.Duration) (float64, error) { return 25, nil }
	recorder := &recordingSpeedRunner{result: protocol.SpeedTest{Timestamp: time.Now().Unix()}}
	daemon.speed = recorder
	if _, err := daemon.runSpeedTest(context.Background(), "household"); err != nil {
		t.Fatal(err)
	}
	if !recorder.traffic.Contaminated || !recorder.traffic.Override {
		t.Fatalf("runner traffic = %+v", recorder.traffic)
	}
}

func TestSpeedTestDeferralHonorsMeteredBatteryAndUnknownCapacitySafety(t *testing.T) {
	t.Parallel()
	capacity := protocol.SpeedProfileConfig{Name: "capacity"}
	content := protocol.SpeedProfileConfig{Name: "content"}
	for _, test := range []struct {
		name    string
		traffic protocol.TrafficContext
		profile protocol.SpeedProfileConfig
		want    string
	}{
		{"metered", protocol.TrafficContext{Metered: true, MeteredAvailable: true, PowerAvailable: true}, content, "metered_connection"},
		{"battery-healthy-charge-runs", protocol.TrafficContext{OnBattery: true, BatteryPct: 61, MeteredAvailable: true, PowerAvailable: true}, content, ""},
		{"battery-low-charge-defers", protocol.TrafficContext{OnBattery: true, BatteryPct: 15, MeteredAvailable: true, PowerAvailable: true}, content, "battery_power"},
		{"battery-unknown-charge-defers", protocol.TrafficContext{OnBattery: true, MeteredAvailable: true, PowerAvailable: true}, content, "battery_power"},
		{"capacity-metering-unknown", protocol.TrafficContext{PowerAvailable: true}, capacity, "metered_status_unavailable"},
		{"capacity-power-unknown", protocol.TrafficContext{MeteredAvailable: true}, capacity, "power_status_unavailable"},
		{"content-allows-explicit-unknown", protocol.TrafficContext{}, content, ""},
		{"override", protocol.TrafficContext{Metered: true, OnBattery: true, Override: true}, capacity, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := speedTestDeferral(test.traffic, test.profile); got != test.want {
				t.Fatalf("deferral = %q, want %q", got, test.want)
			}
		})
	}
}

func TestExperienceEligibilityRejectsIncompleteOrLowEvidenceTests(t *testing.T) {
	t.Parallel()
	valid := protocol.SpeedTest{
		Download: protocol.ThroughputResult{BytesTransferred: 1, ThroughputMbps: 10},
		Upload:   protocol.ThroughputResult{BytesTransferred: 1, ThroughputMbps: 5},
		Responsiveness: protocol.Responsiveness{
			Baseline: protocol.LatencySummary{Count: 1}, DownloadLoaded: protocol.LatencySummary{Count: 1}, UploadLoaded: protocol.LatencySummary{Count: 1},
		},
		Experience: quality.Experience{Available: true}, MeasurementConfidence: quality.Confidence{Level: "high"},
	}
	profile := protocol.SpeedProfileConfig{Name: "content", CountsTowardExperience: true}
	if eligible, reason := experienceEligibility(valid, protocol.TrafficContext{}, profile); !eligible || reason != "" {
		t.Fatalf("valid result excluded: %v %q", eligible, reason)
	}
	for _, test := range []struct {
		name   string
		mutate func(*protocol.SpeedTest)
		want   string
	}{
		{"phase-error", func(result *protocol.SpeedTest) { result.Download.Error = "failed" }, "required_phase_error"},
		{"throughput", func(result *protocol.SpeedTest) { result.Upload.BytesTransferred = 0 }, "throughput_unavailable"},
		{"latency", func(result *protocol.SpeedTest) { result.Responsiveness.Baseline.Count = 0 }, "latency_evidence_unavailable"},
		{"experience", func(result *protocol.SpeedTest) { result.Experience.Available = false }, "experience_unavailable"},
		{"confidence", func(result *protocol.SpeedTest) { result.MeasurementConfidence.Level = "low" }, "low_measurement_confidence"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := valid
			test.mutate(&result)
			if eligible, reason := experienceEligibility(result, protocol.TrafficContext{}, profile); eligible || reason != test.want {
				t.Fatalf("eligibility = %v,%q, want false,%q", eligible, reason, test.want)
			}
		})
	}
}

func TestControllerSpeedRequestValidation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	valid := protocol.SpeedTestRequest{Nonce: "nonce_1234567890", Profile: "content", IssuedAt: now.Add(-time.Minute).Unix(), ExpiresAt: now.Add(2 * time.Minute).Unix()}
	if reason := validateControllerSpeedRequest(valid, now); reason != "" {
		t.Fatalf("valid request rejected: %s", reason)
	}
	valid.ExpiresAt = now.Add(11 * time.Minute).Unix()
	if reason := validateControllerSpeedRequest(valid, now); reason != "expiry_too_long" {
		t.Fatalf("long-lived request reason = %q", reason)
	}
	valid.ExpiresAt = now.Add(2 * time.Minute).Unix()
	valid.Nonce = "unsafe nonce!"
	if reason := validateControllerSpeedRequest(valid, now); reason != "invalid_nonce" {
		t.Fatalf("unsafe nonce reason = %q", reason)
	}
	valid.Nonce = "nonce_1234567890"
	valid.IssuedAt = now.Add(-11 * time.Minute).Unix()
	if reason := validateControllerSpeedRequest(valid, now); reason != "invalid_issued_at" {
		t.Fatalf("stale issue time reason = %q", reason)
	}
}

func TestUploadAndSpeedTrafficAreMutuallyExclusive(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	daemon, err := Open(dir, filepath.Join(dir, "pulse.sock"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Close() }()
	daemon.mu.Lock()
	daemon.speedBusy = true
	daemon.mu.Unlock()
	daemon.startUpload(context.Background())
	daemon.mu.Lock()
	uploadBusy := daemon.uploadBusy
	daemon.speedBusy = false
	daemon.mu.Unlock()
	if uploadBusy {
		t.Fatal("upload started while speed measurement traffic was active")
	}
}

func TestSpeedBudgetReasonNamesBindingLedger(t *testing.T) {
	t.Parallel()
	if reason := speedBudgetReason(10<<20, 0); reason != "monthly_data_budget" {
		t.Fatalf("monthly reason = %q", reason)
	}
	if reason := speedBudgetReason(0, 10<<20); reason != "daily_data_budget" {
		t.Fatalf("daily reason = %q", reason)
	}
}

func TestStandaloneStatusUsesSameEffectiveConfigAsScheduler(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	daemon, err := Open(dir, filepath.Join(dir, "pulse.sock"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Close() }()
	remote := protocol.DefaultConfig()
	remote.DailyDataBudgetMB = 7
	remote.MonthlyDataBudgetMB = 9
	remote.SpeedProfiles[0].CadenceMinutes = 60
	if err := daemon.store.SetEnrollment("https://controller.example", protocol.EnrollmentResponse{
		SensorID: "sensor", SensorSecret: "secret", SiteID: "site", IngestURL: "https://controller.example/ingest",
		Config: remote, ConfigVersion: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if err := daemon.store.SetMode(state.ModeStandalone); err != nil {
		t.Fatal(err)
	}
	status, err := daemon.Status(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	defaults := protocol.DefaultConfig()
	if status.DataBudgetLimit != uint64(defaults.DailyDataBudgetMB)<<20 || status.MonthlyBudgetLimit != uint64(defaults.MonthlyDataBudgetMB)<<20 || status.SpeedProfiles[0].CadenceMinutes != defaults.SpeedProfiles[0].CadenceMinutes {
		t.Fatalf("standalone status leaked retained controller config: %+v", status)
	}
}

func TestScheduledSpeedTestKeepsModeLockUntilResultIsPublished(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	daemon, err := Open(dir, filepath.Join(dir, "pulse.sock"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Close() }()
	if err := daemon.store.SetMode(state.ModeStandalone); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	daemon.speed = &blockingSpeedRunner{started: started, release: release}
	daemon.collector = staticCollector{}
	daemon.crossTraffic = func(context.Context, time.Duration) (float64, error) { return 0, nil }
	daemon.prober = emptyProbe{}
	config := protocol.DefaultConfig()
	seed := daemon.store.Snapshot().SensorUID + ":household"
	due := measurement.NextSpeedProfile(time.Now(), time.Time{}, config.SpeedProfiles[0].Windows, config.SpeedProfiles[0].CadenceMinutes, seed).Add(time.Second)
	daemon.started = due.Add(-10 * time.Minute)
	daemon.lastProbe, daemon.lastDNS, daemon.lastHTTP = due, due, due
	daemon.tick(context.Background(), due)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduled speed test did not start")
	}
	modeDone := make(chan *ipc.Error, 1)
	go func() {
		// Switch away and back through connected is unnecessary: invoking a
		// writer directly proves it cannot cross the in-flight read boundary.
		func() {
			daemon.modeRun.Lock()
			defer daemon.modeRun.Unlock()
		}()
		_, ipcErr := daemon.HandleIPC(context.Background(), ipc.Request{Method: "status"})
		modeDone <- ipcErr
	}()
	select {
	case <-modeDone:
		t.Fatal("mode writer crossed an in-flight scheduled speed test")
	case <-time.After(100 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case ipcErr := <-modeDone:
		if ipcErr != nil {
			t.Fatal(ipcErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("mode writer remained blocked after speed test completion")
	}
}

func TestPseudonymousLinkIdentityIsScopedAndRawValuesStayOffWire(t *testing.T) {
	t.Parallel()
	first := protocol.LinkContext{RawNetworkIdentity: "00:11:22:33:44:55", RawFirstHop: "192.168.1.1"}
	second := first
	applyPseudonymousLinkIdentity(&first, "device-local-key-one")
	applyPseudonymousLinkIdentity(&second, "device-local-key-two")
	if first.NetworkID == "" || first.FirstHopID == "" || first.NetworkID == second.NetworkID || len(first.NetworkID) != 32 {
		t.Fatalf("pseudonyms = %+v / %+v", first, second)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "00:11:22:33:44:55") || strings.Contains(string(encoded), "192.168.1.1") {
		t.Fatalf("raw link identity reached wire JSON: %s", encoded)
	}
}

func (emptyProbe) Latency(context.Context, protocol.SensorConfig, string) []aggregate.Sample {
	return nil
}
func (emptyProbe) DNS(context.Context, protocol.SensorConfig) []protocol.DNSCheck { return nil }
func (emptyProbe) HTTP(context.Context, protocol.SensorConfig, string) []protocol.HTTPCheck {
	return nil
}

func (p *blockingProbe) Latency(_ context.Context, _ protocol.SensorConfig, controllerURL string) []aggregate.Sample {
	p.started <- controllerURL
	<-p.release
	return []aggregate.Sample{{Timestamp: p.sampleAt, Target: "anchor:1.1.1.1", RTTMS: 10, Success: true}}
}
func (*blockingProbe) DNS(context.Context, protocol.SensorConfig) []protocol.DNSCheck { return nil }
func (*blockingProbe) HTTP(context.Context, protocol.SensorConfig, string) []protocol.HTTPCheck {
	return nil
}

func (p *blockingOperationProbe) wait(operation string) {
	if p.operation != operation {
		return
	}
	p.once.Do(func() { close(p.started) })
	<-p.release
}

func (p *blockingOperationProbe) Latency(context.Context, protocol.SensorConfig, string) []aggregate.Sample {
	p.wait("latency")
	return nil
}

func (p *blockingOperationProbe) DNS(context.Context, protocol.SensorConfig) []protocol.DNSCheck {
	p.wait("dns")
	return nil
}

func (p *blockingOperationProbe) HTTP(context.Context, protocol.SensorConfig, string) []protocol.HTTPCheck {
	p.wait("http")
	return nil
}

func (p *cancellationProbe) Latency(ctx context.Context, _ protocol.SensorConfig, _ string) []aggregate.Sample {
	close(p.started)
	<-ctx.Done()
	close(p.done)
	return nil
}

func (*cancellationProbe) DNS(context.Context, protocol.SensorConfig) []protocol.DNSCheck {
	return nil
}

func (*cancellationProbe) HTTP(context.Context, protocol.SensorConfig, string) []protocol.HTTPCheck {
	return nil
}

func (c *blockingCollector) Collect(_ context.Context, now time.Time, includeSSID bool) protocol.LinkContext {
	c.once.Do(func() { close(c.started) })
	<-c.release
	ssid := ""
	if includeSSID {
		ssid = "Private WiFi"
	}
	return protocol.LinkContext{Timestamp: now.Unix(), InterfaceType: "wifi", SSID: ssid}
}

func (staticCollector) Collect(_ context.Context, now time.Time, _ bool) protocol.LinkContext {
	return protocol.LinkContext{Timestamp: now.Unix(), InterfaceType: "ethernet"}
}

func (r *blockingSpeedRunner) RunTest(context.Context, protocol.TrafficContext, uint64, protocol.SpeedProfileConfig, measurement.HouseholdRequest) (protocol.SpeedTest, error) {
	close(r.started)
	<-r.release
	return protocol.SpeedTest{}, nil
}

func (r *recordingSpeedRunner) RunTest(_ context.Context, traffic protocol.TrafficContext, _ uint64, profile protocol.SpeedProfileConfig, _ measurement.HouseholdRequest) (protocol.SpeedTest, error) {
	r.traffic = traffic
	r.profile = profile
	return r.result, nil
}

func (r fakeSpeedRunner) RunTest(context.Context, protocol.TrafficContext, uint64, protocol.SpeedProfileConfig, measurement.HouseholdRequest) (protocol.SpeedTest, error) {
	return r.result, r.err
}

func TestBoundHistoryPayloadKeepsNewestRecordsUnderIPCMessageCap(t *testing.T) {
	t.Parallel()
	payload := json.RawMessage(bytes.Repeat([]byte("x"), 10<<10))
	records := make([]spool.HistoryRecord, 200) // ~2 MB total, twice the IPC cap
	for index := range records {
		records[index] = spool.HistoryRecord{Kind: spool.KindMinuteMetric, Timestamp: int64(index), Data: payload}
	}
	bounded := boundHistoryPayload(records)
	if len(bounded) == 0 || len(bounded) >= len(records) {
		t.Fatalf("bounded %d of %d records", len(bounded), len(records))
	}
	total := 0
	for _, record := range bounded {
		total += len(record.Data) + 96
	}
	if total > 700<<10 {
		t.Fatalf("bounded payload is %d bytes, exceeds the message budget", total)
	}
	if bounded[len(bounded)-1].Timestamp != records[len(records)-1].Timestamp {
		t.Fatal("bounding dropped the newest record instead of the oldest")
	}
}

func TestBoundHistoryPayloadRetainsOversizedNewestRecord(t *testing.T) {
	t.Parallel()
	records := []spool.HistoryRecord{
		{Kind: spool.KindMinuteMetric, Timestamp: 1, Data: json.RawMessage(bytes.Repeat([]byte("x"), 10<<10))},
		{Kind: spool.KindMinuteMetric, Timestamp: 2, Data: json.RawMessage(bytes.Repeat([]byte("y"), 800<<10))},
	}
	bounded := boundHistoryPayload(records)
	if len(bounded) != 1 || bounded[0].Timestamp != 2 {
		t.Fatalf("bounded = %d records, want only the oversized newest record", len(bounded))
	}
}

func TestManualTestUsesDeviceOwnedProfileCeilingsOverRemoteConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	daemon, err := Open(dir, filepath.Join(dir, "pulse.sock"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Close() }()
	if err := daemon.store.SetMode("standalone"); err != nil {
		t.Fatal(err)
	}
	// Remote config schedules deliberately small background tests; the user's
	// own Start button must still measure with the local adaptive ceilings.
	remote := protocol.DefaultConfig()
	remote.SpeedProfiles[0].MaxDownloadBytes = 7 << 20
	remote.SpeedProfiles[0].MaxUploadBytes = 3 << 20
	if err := daemon.store.SetConfig(remote, 2); err != nil {
		t.Fatal(err)
	}
	daemon.collector = staticCollector{}
	daemon.crossTraffic = func(context.Context, time.Duration) (float64, error) { return 0, nil }
	recorder := &recordingSpeedRunner{result: protocol.SpeedTest{Timestamp: time.Now().Unix()}}
	daemon.speed = recorder
	if _, err := daemon.runSpeedTest(context.Background(), "household"); err != nil {
		t.Fatal(err)
	}
	local := protocol.DefaultConfig().SpeedProfiles[0]
	if recorder.profile.MaxDownloadBytes != local.MaxDownloadBytes || recorder.profile.MaxUploadBytes != local.MaxUploadBytes {
		t.Fatalf("manual test used remote ceilings: %+v", recorder.profile)
	}
}

func TestManualTestRunsPastExhaustedLedgerAndChargesOverdraw(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	daemon, err := Open(dir, filepath.Join(dir, "pulse.sock"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Close() }()
	if err := daemon.store.SetMode("standalone"); err != nil {
		t.Fatal(err)
	}
	tiny := protocol.DefaultConfig()
	tiny.DailyDataBudgetMB = 1
	if err := daemon.store.SetConfig(tiny, 2); err != nil {
		t.Fatal(err)
	}
	daemon.collector = staticCollector{}
	daemon.crossTraffic = func(context.Context, time.Duration) (float64, error) { return 0, nil }
	recorder := &recordingSpeedRunner{result: protocol.SpeedTest{Timestamp: time.Now().Unix(), Budget: quality.Budget{UsedBytes: 4 << 20}}}
	daemon.speed = recorder
	if _, err := daemon.runSpeedTest(context.Background(), "household"); err != nil {
		t.Fatalf("manual test was stopped by the ledger: %v", err)
	}
	daily, _ := daemon.store.RemainingDataBudgetsFor(time.Now(), tiny.DailyDataBudgetMB, tiny.MonthlyDataBudgetMB)
	if daily != 0 {
		t.Fatalf("overdraw was not charged: daily remaining = %d", daily)
	}
}

func TestStageUpdateDrainsActivityBeforeDownloadAndReopensOnFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	daemon, err := Open(dir, filepath.Join(dir, "pulse.sock"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Close() }()
	stageCalled := make(chan struct{}, 1)
	daemon.updates = &fakeUpdateManager{stageCalled: stageCalled, stagePath: filepath.Join(dir, "staged")}
	available := pulseupdate.Available{Artifact: pulseupdate.Artifact{Version: "1.2.0", SizeBytes: 1}}
	daemon.updateStatus.Available = &available
	activationFailure := errors.New("injected activation failure")
	daemon.activate = func(context.Context, string, string, string) error {
		return fmt.Errorf("activate staged update: %w", activationFailure)
	}
	measurementAt := time.Now().Truncate(time.Minute).Add(5 * time.Second)
	daemon.aggregator.AddSample(aggregate.Sample{Timestamp: measurementAt, Target: "update.test", RTTMS: 10, Success: true})
	releaseHeld, admitted := daemon.activity.begin()
	if !admitted {
		t.Fatal("initial activity was not admitted")
	}
	done := make(chan *ipc.Error, 1)
	go func() {
		_, ipcErr := daemon.HandleIPC(context.Background(), ipc.Request{Method: "stage_update"})
		done <- ipcErr
	}()
	waitForQuiescence(t, &daemon.activity)
	select {
	case <-stageCalled:
		t.Fatal("update download started before admitted activity drained")
	default:
	}
	releaseHeld()
	select {
	case <-stageCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("update download did not start after activity drained")
	}
	ipcErr := <-done
	if ipcErr == nil || ipcErr.Code != "update_activate_failed" || !strings.Contains(ipcErr.Message, activationFailure.Error()) {
		t.Fatalf("stage error = %+v", ipcErr)
	}
	release, admitted := daemon.activity.begin()
	if !admitted {
		t.Fatal("failed activation left service activity quiesced")
	}
	release()
	if daemon.updateStatus.StagedPath != "" {
		t.Fatalf("failed activation left a staged success marker: %q", daemon.updateStatus.StagedPath)
	}
	records, err := daemon.aggregator.Shutdown(measurementAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	preserved := false
	for _, record := range records {
		preserved = preserved || record.Kind == spool.KindMinuteMetric
	}
	if !preserved {
		t.Fatal("failed activation discarded the open measurement interval")
	}
}

func TestStageUpdateWaitsForEverySchedulerProducer(t *testing.T) {
	for _, operation := range []string{"latency", "dns", "http", "collector", "scheduled_speed"} {
		t.Run(operation, func(t *testing.T) {
			dir := t.TempDir()
			daemon, err := Open(dir, filepath.Join(dir, "pulse.sock"), slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = daemon.Close() }()
			if err := daemon.store.SetMode(state.ModeStandalone); err != nil {
				t.Fatal(err)
			}
			started := make(chan struct{})
			release := make(chan struct{})
			stageCalled := prepareFailingUpdate(t, daemon, dir)
			daemon.collector = staticCollector{}
			daemon.prober = &blockingOperationProbe{operation: operation, started: started, release: release}
			when := time.Now()
			if operation == "collector" {
				daemon.prober = emptyProbe{}
				daemon.collector = &blockingCollector{started: started, release: release}
			}
			if operation == "scheduled_speed" {
				daemon.prober = emptyProbe{}
				daemon.speed = &blockingSpeedRunner{started: started, release: release}
				daemon.crossTraffic = func(context.Context, time.Duration) (float64, error) { return 0, nil }
				config := protocol.DefaultConfig()
				seed := daemon.store.Snapshot().SensorUID + ":household"
				when = measurement.NextSpeedProfile(when, time.Time{}, config.SpeedProfiles[0].Windows, config.SpeedProfiles[0].CadenceMinutes, seed).Add(time.Second)
				daemon.started = when.Add(-10 * time.Minute)
				daemon.lastProbe, daemon.lastDNS, daemon.lastHTTP = when, when, when
			}
			daemon.tick(context.Background(), when)
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatalf("%s producer did not start", operation)
			}
			assertStageWaitsForRelease(t, daemon, stageCalled, release)
			daemon.wg.Wait()
		})
	}
}

func TestStageUpdateWaitsForActiveUpload(t *testing.T) {
	t.Parallel()
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(started) })
		<-release
		_ = json.NewEncoder(w).Encode(protocol.HeartbeatResponse{ConfigVersion: 1})
	}))
	defer server.Close()
	dir := t.TempDir()
	daemon, err := Open(dir, filepath.Join(dir, "pulse.sock"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Close() }()
	secret := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("u", 32)))
	if err := daemon.store.SetEnrollment(server.URL, protocol.EnrollmentResponse{
		SensorID: "sensor", SensorSecret: secret, SiteID: "site", IngestURL: server.URL + protocol.IngestPath,
		Config: protocol.DefaultConfig(), ConfigVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	daemon.client.HTTP = server.Client()
	stageCalled := prepareFailingUpdate(t, daemon, dir)
	daemon.startUpload(context.Background())
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upload did not start")
	}
	assertStageWaitsForRelease(t, daemon, stageCalled, release)
	daemon.wg.Wait()
}

func TestStageUpdateWaitsForActiveEnrollment(t *testing.T) {
	t.Parallel()
	started, release := make(chan struct{}), make(chan struct{})
	secret := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("e", 32)))
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case protocol.EnrollPath:
			close(started)
			<-release
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(protocol.EnrollmentResponse{
				SensorID: "sensor", SensorSecret: secret, SiteID: "site", IngestURL: server.URL + protocol.IngestPath,
				Config: protocol.DefaultConfig(), ConfigVersion: 1,
			})
		case protocol.HeartbeatPath:
			_ = json.NewEncoder(w).Encode(protocol.HeartbeatResponse{ConfigVersion: 1})
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	daemon, err := Open(dir, filepath.Join(dir, "pulse.sock"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Close() }()
	daemon.client.HTTP = server.Client()
	stageCalled := prepareFailingUpdate(t, daemon, dir)
	connectDone := make(chan *ipc.Error, 1)
	params, _ := json.Marshal(map[string]string{"url": server.URL, "token": "spt_test_token_123456"})
	go func() {
		_, ipcErr := daemon.HandleIPC(context.Background(), ipc.Request{Method: "connect", Params: params})
		connectDone <- ipcErr
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("enrollment did not start")
	}
	assertStageWaitsForRelease(t, daemon, stageCalled, release)
	if ipcErr := <-connectDone; ipcErr != nil {
		t.Fatal(ipcErr)
	}
}

func prepareFailingUpdate(t *testing.T, daemon *Service, dir string) chan struct{} {
	t.Helper()
	stageCalled := make(chan struct{}, 1)
	daemon.updates = &fakeUpdateManager{stageCalled: stageCalled, stagePath: filepath.Join(dir, "staged")}
	available := pulseupdate.Available{Artifact: pulseupdate.Artifact{Version: "1.2.0", SizeBytes: 1}}
	daemon.updateStatus.Available = &available
	daemon.activate = func(context.Context, string, string, string) error { return errors.New("injected activation failure") }
	return stageCalled
}

func assertStageWaitsForRelease(t *testing.T, daemon *Service, stageCalled chan struct{}, release chan struct{}) {
	t.Helper()
	stageDone := make(chan *ipc.Error, 1)
	go func() {
		_, ipcErr := daemon.HandleIPC(context.Background(), ipc.Request{Method: "stage_update"})
		stageDone <- ipcErr
	}()
	waitForQuiescence(t, &daemon.activity)
	select {
	case <-stageCalled:
		t.Fatal("update download started before active operation drained")
	default:
	}
	close(release)
	select {
	case <-stageCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("update download did not start after active operation drained")
	}
	if ipcErr := <-stageDone; ipcErr == nil || ipcErr.Code != "update_activate_failed" {
		t.Fatalf("stage error = %+v", ipcErr)
	}
}

func TestStageUpdateBoundsQuiescenceAndReopensAdmission(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	daemon, err := Open(dir, filepath.Join(dir, "pulse.sock"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Close() }()
	stageCalled := make(chan struct{}, 1)
	daemon.updates = &fakeUpdateManager{stageCalled: stageCalled, stagePath: filepath.Join(dir, "staged")}
	available := pulseupdate.Available{Artifact: pulseupdate.Artifact{Version: "1.2.0", SizeBytes: 1}}
	daemon.updateStatus.Available = &available
	daemon.updateDrainWait = 20 * time.Millisecond
	releaseHeld, admitted := daemon.activity.begin()
	if !admitted {
		t.Fatal("initial activity was not admitted")
	}
	result := make(chan *ipc.Error, 1)
	go func() {
		_, ipcErr := daemon.HandleIPC(context.Background(), ipc.Request{Method: "stage_update"})
		result <- ipcErr
	}()
	select {
	case ipcErr := <-result:
		if ipcErr == nil || ipcErr.Code != "update_deferred" {
			t.Fatalf("stage error = %+v", ipcErr)
		}
	case <-time.After(time.Second):
		t.Fatal("stage update did not bound its quiescence wait")
	}
	select {
	case <-stageCalled:
		t.Fatal("stage download ran without a complete quiescence barrier")
	default:
	}
	if release, admitted := daemon.activity.begin(); !admitted {
		t.Fatal("timed-out quiescence did not reopen admission")
	} else {
		release()
	}
	releaseHeld()
}

func TestStageUpdateFlushesBeforeActivationAndStaysQuiescedOnSuccess(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	daemon, err := Open(dir, filepath.Join(dir, "pulse.sock"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Close() }()
	if err := daemon.store.SetMode(state.ModeStandalone); err != nil {
		t.Fatal(err)
	}
	event := protocol.Event{Timestamp: time.Now().Unix(), Type: "before_update", Detail: map[string]any{}}
	if err := daemon.aggregator.AddEvent(event); err != nil {
		t.Fatal(err)
	}
	daemon.updates = &fakeUpdateManager{stagePath: filepath.Join(dir, "staged")}
	available := pulseupdate.Available{Artifact: pulseupdate.Artifact{Version: "1.2.0", SizeBytes: 1}}
	daemon.updateStatus.Available = &available
	daemon.restartDelay = 0
	daemon.activate = func(ctx context.Context, _, _, _ string) error {
		history, err := daemon.spool.History(ctx, time.Unix(0, 0), 100)
		if err != nil {
			return err
		}
		for _, record := range history {
			if record.Kind == spool.KindEvent && strings.Contains(string(record.Data), "before_update") {
				return nil
			}
		}
		return errors.New("activation ran before the final telemetry flush")
	}
	result, ipcErr := daemon.HandleIPC(context.Background(), ipc.Request{Method: "stage_update"})
	if ipcErr != nil {
		t.Fatal(ipcErr)
	}
	status, ok := result.(UpdateStatus)
	if !ok || !status.Restarting {
		t.Fatalf("update result = %#v", result)
	}
	if release, admitted := daemon.activity.begin(); admitted {
		release()
		t.Fatal("successful activation reopened activity before shutdown")
	}
	select {
	case <-daemon.shutdown:
	case <-time.After(2 * time.Second):
		t.Fatal("successful activation did not request shutdown")
	}
	daemon.wg.Wait()
}

func TestRunReturnsFinalFlushFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	socket := shortSocketPath(t, "pulse-service-run-")
	daemon, err := Open(dir, socket, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Close() }()
	if err := daemon.store.SetMode(state.ModeStandalone); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- daemon.Run(ctx) }()
	waitForIPC(t, socket)
	if err := daemon.spool.Close(); err != nil {
		t.Fatal(err)
	}
	if err := daemon.aggregator.AddEvent(protocol.Event{Timestamp: time.Now().Unix(), Type: "shutdown"}); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-runDone:
		if err == nil || !strings.Contains(err.Error(), "flush telemetry during shutdown") {
			t.Fatalf("Run error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not finish after cancellation")
	}
}

func TestUnexpectedIPCExitCancelsServiceOwnedWorkerContext(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	daemon, err := Open(dir, shortSocketPath(t, "pulse-service-ipc-exit-"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Close() }()
	if err := daemon.store.SetMode(state.ModeStandalone); err != nil {
		t.Fatal(err)
	}
	probe := &cancellationProbe{started: make(chan struct{}), done: make(chan struct{})}
	daemon.prober = probe
	daemon.mu.Lock()
	daemon.dnsBusy, daemon.httpBusy, daemon.uploadBusy = true, true, true
	daemon.mu.Unlock()
	runDone := make(chan error, 1)
	go func() { runDone <- daemon.Run(context.Background()) }()
	select {
	case <-probe.started:
	case <-time.After(3 * time.Second):
		t.Fatal("service worker did not start")
	}
	if err := daemon.ipc.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after IPC exit")
	}
	select {
	case <-probe.done:
	default:
		t.Fatal("IPC exit did not cancel the context owned by the service")
	}
}

func TestRunKeepsResourcesOpenAfterIPCHandlerDrainTimeout(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	daemon, err := Open(dir, shortSocketPath(t, "pulse-service-ipc-drain-"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Close() }()
	started, release := make(chan struct{}), make(chan struct{})
	daemon.ipc.Handler = ipc.HandlerFunc(func(_ context.Context, request ipc.Request) (any, *ipc.Error) {
		if request.Method == "status" {
			return map[string]string{"state": "active"}, nil
		}
		close(started)
		<-release
		return nil, nil
	})
	daemon.ipc.DrainTimeout = 20 * time.Millisecond
	runDone := make(chan error, 1)
	go func() { runDone <- daemon.Run(context.Background()) }()
	waitForIPC(t, daemon.ipc.Path)
	callDone := make(chan error, 1)
	go func() { callDone <- ipc.Call(context.Background(), daemon.ipc.Path, "block", nil, nil) }()
	<-started
	if err := daemon.ipc.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-runDone:
		if !errors.Is(err, ipc.ErrDrainTimeout) {
			t.Fatalf("Run error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not enforce IPC handler drain timeout")
	}
	if err := daemon.Close(); !errors.Is(err, ErrUnsafeShutdown) {
		t.Fatalf("Close while IPC handler was active = %v", err)
	}
	close(release)
	<-callDone
	deadline := time.Now().Add(time.Second)
	for daemon.unsafeDrains.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if daemon.unsafeDrains.Load() != 0 {
		t.Fatal("completed IPC drain did not release resource close guard")
	}
}

func TestRunReturnsWorkerDrainTimeout(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	daemon, err := Open(dir, shortSocketPath(t, "pulse-service-drain-"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Close() }()
	daemon.shutdownWait = 20 * time.Millisecond
	release := make(chan struct{})
	daemon.wg.Go(func() { <-release })
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- daemon.Run(ctx) }()
	waitForIPC(t, daemon.ipc.Path)
	cancel()
	select {
	case err := <-runDone:
		if err == nil || !strings.Contains(err.Error(), "drain service workers: timed out") {
			t.Fatalf("Run error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not enforce its worker drain timeout")
	}
	if err := daemon.Close(); !errors.Is(err, ErrUnsafeShutdown) {
		t.Fatalf("Close while worker was active = %v", err)
	}
	close(release)
	daemon.wg.Wait()
	deadline := time.Now().Add(time.Second)
	for daemon.unsafeDrains.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if daemon.unsafeDrains.Load() != 0 {
		t.Fatal("completed worker drain did not release resource close guard")
	}
}

func TestCloseReturnsResourceFailures(t *testing.T) {
	t.Parallel()
	closeFailure := errors.New("injected log close failure")
	daemon := &Service{logCloser: errorCloser{err: closeFailure}}
	err := daemon.Close()
	if !errors.Is(err, closeFailure) || !strings.Contains(err.Error(), "close service log") {
		t.Fatalf("Close error = %v", err)
	}
}

func waitForQuiescence(t *testing.T, gate *activityGate) {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		release, admitted := gate.begin()
		if !admitted {
			return
		}
		release()
		select {
		case <-timer.C:
			t.Fatal("activity admission did not close")
		default:
			runtime.Gosched()
		}
	}
}

func waitForIPC(t *testing.T, socket string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		err := ipc.Call(ctx, socket, "status", nil, nil)
		cancel()
		if err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("service IPC did not become ready")
}

func TestResolveUpdateSourcePrecedence(t *testing.T) {
	t.Setenv("AXON_PULSE_UPDATE_INDEX_URL", "")
	t.Setenv("AXON_PULSE_UPDATE_CHANNEL", "")
	check := func(persisted, wantChannel, wantURL string, wantLocked bool) {
		t.Helper()
		channel, indexURL, locked := resolveUpdateSource(persisted)
		if channel != wantChannel || indexURL != wantURL || locked != wantLocked {
			t.Fatalf("resolveUpdateSource(%q) = %q, %q, %v; want %q, %q, %v", persisted, channel, indexURL, locked, wantChannel, wantURL, wantLocked)
		}
	}
	check("", "main", "https://dist.taurinetech.com/pulse/main/index.json", false)
	check("beta", "beta", "https://dist.taurinetech.com/pulse/beta/index.json", false)
	check("nightly", "main", "https://dist.taurinetech.com/pulse/main/index.json", false)
	t.Setenv("AXON_PULSE_UPDATE_CHANNEL", "alpha")
	check("", "alpha", "https://dist.taurinetech.com/pulse/alpha/index.json", false)
	check("beta", "beta", "https://dist.taurinetech.com/pulse/beta/index.json", false)
	t.Setenv("AXON_PULSE_UPDATE_INDEX_URL", "https://mirror.example/pulse/index.json")
	check("beta", "", "https://mirror.example/pulse/index.json", true)
}

func TestSetUpdateChannelSwapsManagerAndChecksImmediately(t *testing.T) {
	t.Setenv("AXON_PULSE_UPDATE_INDEX_URL", "")
	t.Setenv("AXON_PULSE_UPDATE_CHANNEL", "")
	dir := t.TempDir()
	daemon, err := Open(dir, shortSocketPath(t, "pulse-service-channel-"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = daemon.Close() })
	var built []string
	daemon.newUpdateManager = func(indexURL string) updateManager {
		built = append(built, indexURL)
		return &fakeUpdateManager{}
	}
	daemon.updates = &fakeUpdateManager{}

	result, ipcErr := daemon.HandleIPC(context.Background(), ipc.Request{Method: "set_update_channel", Params: json.RawMessage(`{"channel":"beta"}`)})
	if ipcErr != nil {
		t.Fatalf("set_update_channel: %v", ipcErr)
	}
	status, ok := result.(UpdateStatus)
	if !ok || status.Channel != "beta" || status.ChannelLocked || status.CheckError != "" || !status.CheckedAt.After(time.Time{}) {
		t.Fatalf("unexpected update status after stream change: %+v", result)
	}
	if len(built) != 1 || built[0] != "https://dist.taurinetech.com/pulse/beta/index.json" {
		t.Fatalf("managers built = %v, want the beta index", built)
	}
	if got := daemon.store.Snapshot().UpdateChannel; got != "beta" {
		t.Fatalf("persisted channel = %q, want beta", got)
	}
	full, err := daemon.Status(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if full.UpdateChannel != "beta" || full.UpdateChannelLocked {
		t.Fatalf("status reports channel %q locked=%v", full.UpdateChannel, full.UpdateChannelLocked)
	}

	if _, ipcErr := daemon.HandleIPC(context.Background(), ipc.Request{Method: "set_update_channel", Params: json.RawMessage(`{"channel":"nightly"}`)}); ipcErr == nil || ipcErr.Code != "invalid_parameters" {
		t.Fatalf("invalid channel error = %v, want invalid_parameters", ipcErr)
	}

	t.Setenv("AXON_PULSE_UPDATE_INDEX_URL", "https://mirror.example/pulse/index.json")
	if _, ipcErr := daemon.HandleIPC(context.Background(), ipc.Request{Method: "set_update_channel", Params: json.RawMessage(`{"channel":"main"}`)}); ipcErr == nil || ipcErr.Code != "update_channel_locked" {
		t.Fatalf("locked stream error = %v, want update_channel_locked", ipcErr)
	}
	full, err = daemon.Status(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !full.UpdateChannelLocked || full.UpdateChannel != "" {
		t.Fatalf("locked status = %+v", full)
	}
}

// shortSocketPath returns an IPC endpoint the service can listen on in tests.
// Windows IPC listens on named pipes, which live in the pipe namespace rather
// than the filesystem; elsewhere /tmp keeps the socket path under the
// 104-byte sun_path limit that t.TempDir can exceed on macOS.
func shortSocketPath(t *testing.T, prefix string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return `\\.\pipe\` + prefix + strings.ReplaceAll(t.Name(), "/", "-")
	}
	dir, err := os.MkdirTemp("/tmp", prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "pulse.sock")
}

func TestAPTUpdatesAreManagedWithoutDownloadsOrMeasurementInterruption(t *testing.T) {
	t.Setenv("AXON_PULSE_DISABLE_AUTO_UPDATE", "")
	previousVersion := buildinfo.Version
	buildinfo.Version = "1.0.0"
	t.Cleanup(func() { buildinfo.Version = previousVersion })
	daemon, err := Open(t.TempDir(), shortSocketPath(t, "pulse-apt-"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = daemon.Close() })
	daemon.updateMethod = "apt"
	// Any attempt to check/stage through the built-in manager would panic.
	daemon.updates = nil
	daemon.started = time.Now().Add(-time.Hour)
	daemon.startAutomaticUpdate(context.Background(), time.Now())
	daemon.wg.Wait()
	if daemon.updateBusy || !daemon.lastUpdateCheck.IsZero() {
		t.Fatal("APT build scheduled an automatic update")
	}
	result, ipcErr := daemon.HandleIPC(context.Background(), ipc.Request{Method: "check_update"})
	if ipcErr != nil {
		t.Fatal(ipcErr)
	}
	status, ok := result.(UpdateStatus)
	if !ok || status.UpdateMethod != "apt" || !status.ChannelLocked || status.Channel != "" || status.AvailableVersion != "" || !status.CheckedAt.IsZero() {
		t.Fatalf("unexpected APT update status: %+v", result)
	}
	for _, method := range []string{"stage_update", "set_update_channel"} {
		_, ipcErr := daemon.HandleIPC(context.Background(), ipc.Request{Method: method, Params: json.RawMessage(`{"channel":"alpha"}`)})
		if ipcErr == nil || ipcErr.Code != "update_package_managed" {
			t.Fatalf("%s error = %v", method, ipcErr)
		}
	}
	if daemon.updateBusy || daemon.store.Snapshot().UpdateChannel == "alpha" {
		t.Fatal("APT manual update changed service activity or saved channel")
	}
	full, err := daemon.Status(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if full.UpdateMethod != "apt" || !full.UpdateChannelLocked || full.UpdateChannel != "" {
		t.Fatalf("unexpected APT service status: %+v", full)
	}
}
