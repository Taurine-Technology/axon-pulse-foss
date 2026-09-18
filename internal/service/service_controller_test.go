package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Taurine-Technology/axon-pulse/internal/protocol"
	"github.com/Taurine-Technology/axon-pulse/internal/spool"
	"github.com/Taurine-Technology/axon-pulse/quality"
)

type (
	// lowBatteryCollector reports a laptop unplugged at a charge the
	// scheduler refuses to drain.
	lowBatteryCollector struct{}
)

func (lowBatteryCollector) Collect(_ context.Context, now time.Time, _ bool) protocol.LinkContext {
	link := protocol.LinkContext{Timestamp: now.Unix(), InterfaceType: "wifi", OnBattery: true, BatteryPct: 8}
	link.Availability.Power = true
	return link
}

func pendingEvents(t *testing.T, daemon *Service) []protocol.Event {
	t.Helper()
	var events []protocol.Event
	for _, record := range daemon.aggregator.FlushPending() {
		if record.Kind != spool.KindEvent {
			continue
		}
		var event protocol.Event
		if err := json.Unmarshal(record.Payload, &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

// spooledEvents reads events already flushed to the durable spool.
func spooledEvents(t *testing.T, daemon *Service) []protocol.Event {
	t.Helper()
	records, err := daemon.spool.History(context.Background(), time.Now().Add(-time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	var events []protocol.Event
	for _, record := range records {
		if record.Kind != spool.KindEvent {
			continue
		}
		var event protocol.Event
		if err := json.Unmarshal(record.Data, &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

// A console operator's one-off is explicit intent: it runs on low battery
// past an exhausted ledger, like the device's own Run button, but measures
// with the controller's configured ceilings rather than the device's.
func TestControllerOneOffOverridesGatesWithConfiguredCeilings(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	daemon, err := Open(dir, filepath.Join(dir, "pulse.sock"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Close() }()
	// Connected mode: the controller's config is the effective one.
	remote := protocol.DefaultConfig()
	remote.DailyDataBudgetMB = 1
	remote.SpeedProfiles[0].MaxDownloadBytes = 7 << 20
	remote.SpeedProfiles[0].MaxUploadBytes = 3 << 20
	if err := daemon.store.SetEnrollment("https://controller.example", protocol.EnrollmentResponse{
		SensorID: "sensor", SensorSecret: "secret", SiteID: "site", IngestURL: "https://controller.example/ingest",
		Config: remote, ConfigVersion: 2,
	}); err != nil {
		t.Fatal(err)
	}
	daemon.collector = lowBatteryCollector{}
	daemon.crossTraffic = func(context.Context, time.Duration) (float64, error) { return 0, nil }
	recorder := &recordingSpeedRunner{result: protocol.SpeedTest{Timestamp: time.Now().Unix(), Budget: quality.Budget{UsedBytes: 4 << 20}}}
	daemon.speed = recorder
	daemon.modeRun.RLock()
	defer daemon.modeRun.RUnlock()

	// The scheduler must still defer under the same conditions.
	_, err = daemon.runSpeedTestLocked(context.Background(), triggerScheduled, protocol.ProfileHousehold)
	var deferred *speedDeferredError
	if !errors.As(err, &deferred) || deferred.reason != "battery_power" {
		t.Fatalf("scheduled run on low battery: err=%v", err)
	}

	if _, err := daemon.runSpeedTestLocked(context.Background(), triggerController, protocol.ProfileHousehold); err != nil {
		t.Fatalf("controller one-off was gated: %v", err)
	}
	if !recorder.traffic.Override || !recorder.traffic.OnBattery {
		t.Fatalf("one-off did not annotate and override live conditions: %+v", recorder.traffic)
	}
	if recorder.profile.MaxDownloadBytes != 7<<20 || recorder.profile.MaxUploadBytes != 3<<20 {
		t.Fatalf("one-off ignored the configured ceilings: %+v", recorder.profile)
	}
	daily, _ := daemon.store.RemainingDataBudgetsFor(time.Now(), remote.DailyDataBudgetMB, remote.MonthlyDataBudgetMB)
	if daily != 0 {
		t.Fatalf("overdraw was not charged: daily remaining = %d", daily)
	}

	// The ledger is now genuinely exhausted: the scheduler defers on it,
	// and a second one-off still runs and charges further overdraw.
	_, err = daemon.runSpeedTestLocked(context.Background(), triggerScheduled, protocol.ProfileHousehold)
	if !errors.As(err, &deferred) || deferred.reason != "daily_data_budget" {
		t.Fatalf("scheduled run past the ledger: err=%v", err)
	}
	if _, err := daemon.runSpeedTestLocked(context.Background(), triggerController, protocol.ProfileHousehold); err != nil {
		t.Fatalf("controller one-off was stopped by the ledger: %v", err)
	}
}

// The controller resolves a delivered directive from exactly one verdict
// event, so the verdict must carry the real outcome and its reason.
func TestControllerVerdictFollowsRunOutcome(t *testing.T) {
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
	now := time.Now()
	daemon.resolveControllerSpeedTest(context.Background(), now, true, protocol.ProfileHousehold, "m-0123456789abcdef", nil)
	daemon.resolveControllerSpeedTest(context.Background(), now, true, protocol.ProfileHousehold, "", &speedDeferredError{reason: "daily_data_budget"})
	daemon.resolveControllerSpeedTest(context.Background(), now, true, protocol.ProfileSaturation, "m-ignored-on-failure", errors.New("dial tcp: connection refused"))
	// Each verdict is flushed immediately, so it must already be durable.
	if pending := pendingEvents(t, daemon); len(pending) != 0 {
		t.Fatalf("verdicts left in memory: %+v", pending)
	}
	events := spooledEvents(t, daemon)
	want := []struct{ eventType, profile, reason, measurement string }{
		{"controller_speed_test_accepted", protocol.ProfileHousehold, "", "m-0123456789abcdef"},
		{"controller_speed_test_rejected", protocol.ProfileHousehold, "daily_data_budget", ""},
		{"controller_speed_test_rejected", protocol.ProfileSaturation, "endpoint_or_network_failure", ""},
	}
	if len(events) != len(want) {
		t.Fatalf("events = %+v", events)
	}
	for i, expected := range want {
		event := events[i]
		reason, _ := event.Detail["reason"].(string)
		measurement, _ := event.Detail["measurement_id"].(string)
		if event.Type != expected.eventType || event.Detail["profile"] != expected.profile || reason != expected.reason || measurement != expected.measurement {
			t.Fatalf("event %d = %+v, want %+v", i, event, expected)
		}
	}
}
