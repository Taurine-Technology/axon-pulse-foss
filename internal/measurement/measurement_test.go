package measurement

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Taurine-Technology/axon-contracts/gen/go/diagnostics"
	"github.com/Taurine-Technology/axon-pulse/internal/protocol"
	"github.com/Taurine-Technology/axon-pulse/quality"
)

func TestSpeedRunnerUsesBoundedContentProfile(t *testing.T) {
	t.Parallel()
	var options diagnostics.SpeedTestOptions
	runner := SpeedRunner{Run: func(_ context.Context, got diagnostics.SpeedTestOptions) diagnostics.SpeedTestResult {
		options = got
		return diagnostics.SpeedTestResult{
			Download: diagnostics.ThroughputResult{BytesTransferred: 1, ThroughputMbps: 1}, Upload: diagnostics.ThroughputResult{BytesTransferred: 1, ThroughputMbps: 1},
			Responsiveness: diagnostics.ResponsivenessResult{Baseline: diagnostics.LatencySummary{Count: 1}, DownloadLoaded: diagnostics.LatencySummary{Count: 1}, UploadLoaded: diagnostics.LatencySummary{Count: 1}},
		}
	}}

	profile := protocol.DefaultConfig().SpeedProfiles[1]
	if _, err := runner.RunTest(context.Background(), protocol.TrafficContext{}, 2<<30, profile, HouseholdRequest{}); err != nil {
		t.Fatalf("RunTest: %v", err)
	}
	if options.MaxDownloadBytes != profile.MaxDownloadBytes {
		t.Fatalf("download budget = %d, want %d", options.MaxDownloadBytes, profile.MaxDownloadBytes)
	}
	if options.MaxDurationSeconds != uint32(profile.MaxDurationSeconds) {
		t.Fatalf("duration = %d, want %d", options.MaxDurationSeconds, profile.MaxDurationSeconds)
	}

	// A drained ledger shrinks the effective envelope proportionally instead
	// of deferring outright or exceeding the remaining allowance.
	remaining := uint64(150 << 20)
	if _, err := runner.RunTest(context.Background(), protocol.TrafficContext{}, remaining, profile, HouseholdRequest{}); err != nil {
		t.Fatalf("RunTest with drained ledger: %v", err)
	}
	if uint64(options.MaxDownloadBytes)+uint64(options.MaxUploadBytes) > remaining {
		t.Fatalf("effective envelope %d+%d exceeds remaining %d", options.MaxDownloadBytes, options.MaxUploadBytes, remaining)
	}
	if options.MaxDownloadBytes >= profile.MaxDownloadBytes {
		t.Fatalf("drained ledger did not shrink download budget: %d", options.MaxDownloadBytes)
	}
}

func TestResponsivenessGoldenConversion(t *testing.T) {
	t.Parallel()
	input := diagnostics.SpeedTestResult{
		Download:     diagnostics.ThroughputResult{BytesTransferred: 10_000_000, DurationMS: 1000, ThroughputMbps: 80},
		Upload:       diagnostics.ThroughputResult{BytesTransferred: 5_000_000, DurationMS: 1000, ThroughputMbps: 40},
		Latency:      diagnostics.LatencyResult{MinMS: 10, AvgMS: 20, MaxMS: 30, JitterMS: 2, PacketLossValid: true, ProbesSent: 20},
		EndpointHost: "speed.example", DurationMS: 2500,
		Responsiveness: diagnostics.ResponsivenessResult{
			Baseline:           diagnostics.LatencySummary{Count: 20, MinMS: 10, P5MS: 11, P50MS: 20, P90MS: 25, P95MS: 27, P99MS: 29, MaxMS: 30, PacketLossValid: true},
			DownloadLoaded:     diagnostics.LatencySummary{Count: 10, MinMS: 20, P5MS: 21, P50MS: 40, P90MS: 51, P95MS: 55, P99MS: 58, MaxMS: 60, PacketLossValid: true},
			UploadLoaded:       diagnostics.LatencySummary{Count: 10, MinMS: 25, P5MS: 26, P50MS: 100, P90MS: 211, P95MS: 220, P99MS: 225, MaxMS: 230, PacketLossValid: true},
			DownloadBloatP90MS: 40, UploadBloatP90MS: 200, DownloadGrade: "B", UploadGrade: "D", OverallGrade: "D", PrimaryDriver: "upload_bufferbloat", ConfidenceLevel: "high", ConfidenceReasons: []string{}, Profile: "standard",
		},
	}
	profile := protocol.DefaultConfig().SpeedProfiles[1]
	got := convertSpeedResult(time.Unix(1_800_000_000, 0), input, protocol.TrafficContext{}, profile, quality.Budget{EffectiveDownloadBytes: profile.MaxDownloadBytes, EffectiveUploadBytes: profile.MaxUploadBytes})
	if got.Responsiveness.OverallGrade != "D" || got.Responsiveness.PrimaryDriver != "upload" || got.Download.ThroughputMbps != 80 {
		t.Fatalf("conversion drifted: %+v", got)
	}
	encoded, err := json.Marshal(got)
	if err != nil || !json.Valid(encoded) {
		t.Fatalf("invalid wire JSON: %s %v", encoded, err)
	}
}

func TestSpeedWindowDueOncePerWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 15, 7, 30, 0, 0, time.UTC)
	windows := []string{"00:00-06:00", "06:00-12:00"}
	scheduled, start, ok := speedWindowSchedule(now, windows[1], "sensor-1")
	if !ok || scheduled.Before(start) || !scheduled.Before(time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("invalid randomized schedule: %v, %v, %v", scheduled, start, ok)
	}
	dueAt := scheduled.Add(time.Second)
	if !SpeedWindowDue(dueAt, time.Time{}, windows, "sensor-1") {
		t.Fatal("fresh window should be due after its stable randomized instant")
	}
	if SpeedWindowDue(dueAt, dueAt.Add(-time.Minute), windows, "sensor-1") {
		t.Fatal("completed window should not be due")
	}
}

func TestNextSpeedWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 15, 5, 30, 0, 0, time.Local)
	windows := []string{"00:00-06:00", "06:00-12:00", "12:00-18:00", "18:00-24:00"}
	next := NextSpeedWindow(now, time.Time{}, windows, "sensor-1")
	if next.Before(now) || next.After(now.Add(13*time.Hour)) {
		t.Fatalf("next speed window outside expected range: %v", next)
	}
	completed := next.Add(time.Second)
	following := NextSpeedWindow(completed, completed, windows, "sensor-1")
	if !following.After(completed) {
		t.Fatalf("expected a later unsatisfied window, got %v", following)
	}
}

func TestCadenceRunsInsideAllowedHoursAndOnlyOncePerSlot(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 15, 2, 0, 0, 0, time.UTC)
	windows := []string{"06:00-22:00"}
	next := NextSpeedProfile(now, time.Time{}, windows, 240, "sensor-1:content")
	if next.IsZero() || next.Hour() < 6 || next.Hour() >= 22 {
		t.Fatalf("next cadence escaped allowed hours: %v", next)
	}
	due := next.Add(time.Second)
	if !SpeedProfileDue(due, time.Time{}, windows, 240, "sensor-1:content") {
		t.Fatal("cadence was not due at its stable randomized instant")
	}
	if SpeedProfileDue(due, next, windows, 240, "sensor-1:content") {
		t.Fatal("completed cadence slot ran twice")
	}
}

func TestNeverScheduleStaysDisabled(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	if SpeedProfileDue(now, time.Time{}, []string{"never"}, 60, "sensor-1:capacity") {
		t.Fatal("never profile became due")
	}
	if next := NextSpeedProfile(now, time.Time{}, []string{"never"}, 60, "sensor-1:capacity"); !next.IsZero() {
		t.Fatalf("never profile reported next run %v", next)
	}
}

func TestCadenceAndQuietHoursStayOnWallClockAcrossDST(t *testing.T) {
	t.Parallel()
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, now := range []time.Time{
		time.Date(2026, 3, 8, 0, 30, 0, 0, location),  // spring forward
		time.Date(2026, 11, 1, 0, 30, 0, 0, location), // fall back
	} {
		next := NextSpeedProfile(now, time.Time{}, []string{"06:00-22:00"}, 240, "dst-sensor")
		if next.IsZero() || next.Hour() < 6 || next.Hour() >= 22 {
			t.Fatalf("DST schedule escaped local allowed hours: now=%v next=%v", now, next)
		}
	}
	overnight := time.Date(2026, 3, 8, 1, 30, 0, 0, location)
	next := NextSpeedProfile(overnight, time.Time{}, []string{"22:00-06:00"}, 60, "dst-overnight")
	if next.IsZero() || (next.Hour() >= 6 && next.Hour() < 22) {
		t.Fatalf("DST overnight schedule escaped its window: %v", next)
	}
}

func TestLinkAvailabilityAndAssociationChangesDegradeCleanly(t *testing.T) {
	t.Parallel()
	collector := &Collector{}
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	first := collector.collectLocal(LocalContext{
		InterfaceType: "wifi", ContextAvailable: true, FrequencyMHz: 5180, Channel: 36,
		RSSIDBm: -55, PHY: "802.11ax", NetworkIdentity: "ap-one", Gateway: "192.168.1.1",
	}, now)
	if !first.Availability.RSSI || !first.Availability.Frequency || !first.Availability.Association || first.AssociationChanged {
		t.Fatalf("first availability = %+v", first)
	}
	missing := collector.collectLocal(LocalContext{InterfaceType: "unknown"}, now.Add(time.Second))
	if missing.ContextAvailable || missing.AssociationChanged || missing.Availability.RSSI {
		t.Fatalf("missing telemetry became a change or zero metric: %+v", missing)
	}
	changed := collector.collectLocal(LocalContext{
		InterfaceType: "wifi", ContextAvailable: true, FrequencyMHz: 5955, Channel: 1,
		RSSIDBm: -60, PHY: "802.11be", NetworkIdentity: "ap-two", Gateway: "192.168.1.1",
	}, now.Add(2*time.Second))
	if !changed.AssociationChanged {
		t.Fatalf("valid association change was missed: %+v", changed)
	}
}

func TestMeteredStatusParsingNeverFabricatesUnknownValues(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		value              string
		metered, available bool
	}{{"yes", true, true}, {"guess-no", false, true}, {"Variable", true, true}, {"Unrestricted", false, true}, {"unknown", false, false}, {"", false, false}} {
		metered, available := parseMeteredValue(test.value)
		if metered != test.metered || available != test.available {
			t.Errorf("parseMeteredValue(%q) = %v,%v", test.value, metered, available)
		}
	}
}

// The controller drops a speed-test record whose confidence reasons encode
// as null or whose primary driver is not download/upload, after acking the
// batch. Every household and saturation result from the first alpha
// releases was lost this way.
func TestHardenWireMatchesControllerIngestContract(t *testing.T) {
	t.Parallel()
	result := protocol.SpeedTest{}
	result.Responsiveness.PrimaryDriver = "household"
	hardenWire(&result)
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"confidence_reasons":[]`)) || !bytes.Contains(encoded, []byte(`"reasons":[]`)) {
		t.Fatalf("empty reasons encoded as null: %s", encoded)
	}
	if bytes.Contains(encoded, []byte(`"primary_driver"`)) {
		t.Fatalf("household driver leaked onto the wire: %s", encoded)
	}
	result.Responsiveness.PrimaryDriver = "upload"
	result.Responsiveness.ConfidenceReasons = []string{"cross_traffic_detected"}
	hardenWire(&result)
	if result.Responsiveness.PrimaryDriver != "upload" || len(result.Responsiveness.ConfidenceReasons) != 1 {
		t.Fatalf("hardening altered valid fields: %+v", result.Responsiveness)
	}
}
