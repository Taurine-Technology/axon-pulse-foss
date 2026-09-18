package quality

import (
	"math"
	"slices"
	"testing"
)

func TestDetectStableRegionExcludesRampAndStreamChanges(t *testing.T) {
	t.Parallel()
	buckets := []Bucket{
		{Index: 0, DurationMS: 1000, Bytes: 1_000_000, Samples: 4, StreamCount: 1, Complete: true},
		{Index: 1, DurationMS: 1000, Bytes: 4_800_000, Samples: 4, StreamCount: 4, Complete: true},
		{Index: 2, DurationMS: 1000, Bytes: 5_000_000, Samples: 4, StreamCount: 4, Complete: true},
		{Index: 3, DurationMS: 1000, Bytes: 5_100_000, Samples: 4, StreamCount: 4, Complete: true},
		{Index: 4, DurationMS: 1000, Bytes: 5_000_000, Samples: 4, StreamCount: 4, Complete: true},
	}
	got := DetectStableRegion(buckets, DefaultStabilityPolicy())
	if !got.Stable || got.StartBucket != 1 || got.EndBucket != 4 || got.BucketCount != 4 {
		t.Fatalf("stable region = %+v", got)
	}
	if math.Abs(got.ThroughputMbps-40) > 0.001 {
		t.Fatalf("steady throughput = %f, want 40", got.ThroughputMbps)
	}
}

func TestDetectStableRegionRejectsIncompleteAndUnstableBuckets(t *testing.T) {
	t.Parallel()
	buckets := []Bucket{
		{Index: 0, DurationMS: 1000, Bytes: 1_000_000, Samples: 1, StreamCount: 2, Complete: true},
		{Index: 1, DurationMS: 1000, Bytes: 8_000_000, Samples: 1, StreamCount: 2, Complete: true},
		{Index: 2, DurationMS: 1000, Bytes: 2_000_000, Samples: 1, StreamCount: 2, Complete: true},
		{Index: 3, DurationMS: 400, Bytes: 8_000_000, Samples: 1, StreamCount: 2, Complete: false},
	}
	got := DetectStableRegion(buckets, DefaultStabilityPolicy())
	if got.Stable || got.Reason != "throughput_did_not_stabilize" || got.BucketCount != 3 {
		t.Fatalf("unstable region = %+v", got)
	}
}

func TestApplicationScoresSeparateQualityFromMeasurementConfidence(t *testing.T) {
	t.Parallel()
	input := ApplicationInput{
		Baseline: availablePhase(45, .1), Download: availablePhase(70, .3),
		Upload: availablePhase(90, .3), Bidirectional: availablePhase(65, .4),
		DownloadP75Mbps: 100, UploadP75Mbps: 20, DownloadAvailable: true, UploadAvailable: true,
	}
	got := ScoreApplications(input)
	if got.Rating != RatingGood || len(got.Home) != 4 || len(got.Additional) != 2 {
		t.Fatalf("experience = %+v", got)
	}
	if got.Home["gaming"].Score < 80 || got.Home["streaming"].Score < 80 {
		t.Fatalf("unexpected home scores = %+v", got.Home)
	}

	confidence := GradeConfidence(ConfidenceInput{
		BaselineSamples: 3, PhaseSamples: map[string]int{"download": 3, "upload": 3, "bidirectional": 3},
		StablePhases: map[string]bool{}, ProbeSuccessRatio: .5,
	})
	if confidence.Level != "low" || got.Rating != RatingGood {
		t.Fatalf("quality and confidence were conflated: experience=%+v confidence=%+v", got, confidence)
	}
}

func TestThroughputFloorIsSoftForLatencySensitiveApps(t *testing.T) {
	t.Parallel()
	got := ScoreApplications(ApplicationInput{
		Baseline: availablePhase(30, 0), Download: availablePhase(30, 0),
		Upload: availablePhase(30, 0), Bidirectional: availablePhase(30, 0),
		DownloadP75Mbps: 1, UploadP75Mbps: .4, DownloadAvailable: true, UploadAvailable: true,
	})
	if got.Home["gaming"].Score <= 0 || got.Home["gaming"].Score >= 100 {
		t.Fatalf("gaming soft-floor score = %+v", got.Home["gaming"])
	}
}

func TestThroughputComponentUsesPublishedMinimumAndGoodThresholds(t *testing.T) {
	t.Parallel()
	minimum := ScoreApplications(ApplicationInput{
		Baseline: availablePhase(60, .5), Download: availablePhase(90, 1),
		Upload: availablePhase(180, 2), Bidirectional: availablePhase(70, .8),
		DownloadP75Mbps: 1.5, UploadP75Mbps: 1.5, DownloadAvailable: true, UploadAvailable: true,
	})
	if minimum.Home["video_calls"].ThroughputScore != 0 {
		t.Fatalf("throughput at the published minimum scored %f, want 0", minimum.Home["video_calls"].ThroughputScore)
	}
	good := ScoreApplications(ApplicationInput{
		Baseline: availablePhase(60, .5), Download: availablePhase(90, 1),
		Upload: availablePhase(180, 2), Bidirectional: availablePhase(70, .8),
		DownloadP75Mbps: 4, UploadP75Mbps: 4, DownloadAvailable: true, UploadAvailable: true,
	})
	if good.Home["video_calls"].ThroughputScore != 100 {
		t.Fatalf("throughput at the published good target scored %f, want 100", good.Home["video_calls"].ThroughputScore)
	}
}

func TestMissingPhaseEvidenceIsUnavailableInsteadOfPerfect(t *testing.T) {
	t.Parallel()
	got := ScoreApplications(ApplicationInput{
		Baseline: availablePhase(45, .1), Download: availablePhase(60, .2), Upload: availablePhase(70, .3),
		DownloadP75Mbps: 50, UploadP75Mbps: 10, DownloadAvailable: true, UploadAvailable: true,
	})
	for _, key := range []string{"video_calls", "gaming"} {
		if got.Home[key].Available || got.Home[key].Rating != RatingUnavailable {
			t.Fatalf("%s missing bidirectional evidence scored as available: %+v", key, got.Home[key])
		}
	}
	if !got.Home["browsing"].Available || !got.Home["streaming"].Available {
		t.Fatalf("available outcomes were lost: %+v", got.Home)
	}
}

func TestMissingThroughputMakesDependentApplicationsUnavailable(t *testing.T) {
	t.Parallel()
	got := ScoreApplications(ApplicationInput{Baseline: availablePhase(45, .1)})
	browsing := got.Home["browsing"]
	if browsing.Available || browsing.Rating != RatingUnavailable || browsing.ThroughputScore != 0 {
		t.Fatalf("browsing without throughput = %+v", browsing)
	}
}

func TestStarvedLoadedPhasesAreUnavailableInsteadOfConfidentlyBad(t *testing.T) {
	t.Parallel()
	// A byte-budget-capped phase on a fast link ends with a handful of noisy
	// probes; three terrible samples must not produce a "bad" verdict.
	starved := PhaseMetrics{P95LatencyMS: 400, LossPct: 30, LatencySamples: MinimumLoadedSamples - 2, LossAvailable: true}
	input := ApplicationInput{
		Baseline: availablePhase(20, 0), Download: starved, Upload: starved, Bidirectional: starved,
		DownloadP75Mbps: 90, UploadP75Mbps: 80, DownloadAvailable: true, UploadAvailable: true,
	}
	got := ScoreApplications(input)
	if !got.Home["browsing"].Available {
		t.Fatalf("baseline-backed browsing should still score: %+v", got.Home["browsing"])
	}
	for _, key := range []string{"streaming", "video_calls", "gaming"} {
		app := got.Home[key]
		if app.Available {
			t.Fatalf("%s scored from %d loaded samples: %+v", key, starved.LatencySamples, app)
		}
		if !slices.Contains(app.UnavailableReasons, "insufficient_load_samples") {
			t.Fatalf("%s missing starvation reason: %+v", key, app.UnavailableReasons)
		}
	}
	enough := PhaseMetrics{P95LatencyMS: 40, LossPct: 0, LatencySamples: MinimumLoadedSamples, LossAvailable: true}
	input.Download, input.Upload, input.Bidirectional = enough, enough, enough
	scored := ScoreApplications(input)
	for _, key := range []string{"streaming", "video_calls", "gaming"} {
		if !scored.Home[key].Available {
			t.Fatalf("%s should score at exactly %d samples: %+v", key, MinimumLoadedSamples, scored.Home[key])
		}
	}
}

func availablePhase(latency, loss float64) PhaseMetrics {
	return PhaseMetrics{P95LatencyMS: latency, LossPct: loss, LatencySamples: 20, LossAvailable: true}
}

func TestBrowsingRequiresEnoughIdleEvidence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		samples   int
		latencyMS float64
		rating    Rating
	}{
		{name: "short spiky baseline", samples: 10, latencyMS: 214, rating: RatingUnavailable},
		{name: "short fast baseline", samples: 19, latencyMS: 40, rating: RatingUnavailable},
		{name: "enough fast samples", samples: 20, latencyMS: 40, rating: RatingGood},
		{name: "sustained slow responses", samples: 20, latencyMS: 214, rating: RatingBad},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			baseline := availablePhase(test.latencyMS, 0)
			baseline.LatencySamples = test.samples
			result := ScoreApplications(ApplicationInput{
				Baseline: baseline, Download: availablePhase(40, 0),
				Upload: availablePhase(40, 0), Bidirectional: availablePhase(40, 0),
				DownloadP75Mbps: 100, UploadP75Mbps: 90, DownloadAvailable: true, UploadAvailable: true,
			})
			browsing := result.Home["browsing"]
			if browsing.Rating != test.rating || browsing.Available != (test.rating != RatingUnavailable) {
				t.Fatalf("browsing = %+v, want %s", browsing, test.rating)
			}
			insufficient := test.samples < MinimumBaselineSamples
			if slices.Contains(browsing.UnavailableReasons, "insufficient_baseline_samples") != insufficient {
				t.Fatalf("baseline evidence reason = %v", browsing.UnavailableReasons)
			}
			if result.Home["video_calls"].Rating != RatingGood || result.Home["streaming"].Rating != RatingGood {
				t.Fatalf("idle evidence changed loaded outcomes: %+v", result.Home)
			}
			confidence := GradeConfidence(ConfidenceInput{
				BaselineSamples: test.samples, ProbeSuccessRatio: 1,
				PhaseSamples: map[string]int{"download": 20, "upload": 20, "bidirectional": 20},
				StablePhases: map[string]bool{"download": true, "upload": true, "bidirectional": true},
			})
			if slices.Contains(confidence.Reasons, "baseline_insufficient_samples") != insufficient {
				t.Fatalf("baseline confidence = %+v", confidence)
			}
		})
	}
}

func TestConfidencePolicyReasonsRemainObservable(t *testing.T) {
	t.Parallel()
	base := ConfidenceInput{
		BaselineSamples: 20, PhaseSamples: map[string]int{"download": 20, "upload": 20, "bidirectional": 20},
		StablePhases:      map[string]bool{"download": true, "upload": true, "bidirectional": true},
		ProbeSuccessRatio: 1, FreshPathSuccessRatio: 1,
	}
	tests := []struct {
		name, reason string
		mutate       func(*ConfidenceInput)
	}{
		{"cpu", "agent_cpu_busy_high", func(input *ConfidenceInput) { input.AgentCPUBusyPct = 90 }},
		{"endpoint", "traffic_endpoints_inconsistent", func(input *ConfidenceInput) { input.EndpointCV = .3 }},
		{"source", "measurement_sources_disagree", func(input *ConfidenceInput) { input.SourceDisagreementPct = 30 }},
		{"network", "network_changed_during_test", func(input *ConfidenceInput) { input.NetworkChanged = true }},
		{"ip-family", "ip_family_changed_during_test", func(input *ConfidenceInput) { input.IPFamilyChanged = true }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := base
			test.mutate(&input)
			got := GradeConfidence(input)
			found := false
			for _, reason := range got.Reasons {
				found = found || reason == test.reason
			}
			if !found {
				t.Fatalf("confidence reasons=%v, want %s", got.Reasons, test.reason)
			}
		})
	}
}
