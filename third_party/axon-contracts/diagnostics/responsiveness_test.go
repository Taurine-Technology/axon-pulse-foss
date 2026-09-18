package diagnostics

import (
	"context"
	"math"
	"slices"
	"sync"
	"testing"
)

func TestPercentileUsesLinearInterpolation(t *testing.T) {
	tests := []struct {
		name    string
		values  []float64
		percent float64
		want    float64
	}{
		{name: "empty", values: []float64{}, percent: 90, want: 0},
		{name: "minimum", values: []float64{30, 10, 20}, percent: 0, want: 10},
		{name: "median", values: []float64{40, 10, 30, 20}, percent: 50, want: 25},
		{name: "p90", values: []float64{1, 2, 3, 4, 5}, percent: 90, want: 4.6},
		{name: "maximum", values: []float64{30, 10, 20}, percent: 100, want: 30},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := percentile(tt.values, tt.percent); math.Abs(got-tt.want) > 0.0001 {
				t.Fatalf("percentile(%v, %.1f) = %.4f, want %.4f", tt.values, tt.percent, got, tt.want)
			}
		})
	}
}

func TestLatencySummaryCalculatesJitterMetrics(t *testing.T) {
	summary := latencySummary(LatencyResult{
		PacketLossPct:   25,
		PacketLossValid: true,
		SamplesMS:       []float64{10, 12, 14, 18},
	})

	if summary.Count != 4 || summary.MinMS != 10 || summary.MaxMS != 18 {
		t.Fatalf("summary bounds = %+v", summary)
	}
	if summary.JitterIQRMS != 3.5 {
		t.Fatalf("jitter IQR = %.3f, want 3.5", summary.JitterIQRMS)
	}
	if summary.JitterMeanDeltaMS != 2.667 {
		t.Fatalf("mean delta = %.3f, want 2.667", summary.JitterMeanDeltaMS)
	}
}

func TestBufferbloatGradeThresholds(t *testing.T) {
	tests := []struct {
		name  string
		delta float64
		want  string
	}{
		{name: "A+ below 5", delta: 4.999, want: "A+"},
		{name: "A begins at 5", delta: 5, want: "A"},
		{name: "B begins at 30", delta: 30, want: "B"},
		{name: "C begins at 60", delta: 60, want: "C"},
		{name: "D begins at 200", delta: 200, want: "D"},
		{name: "F begins at 400", delta: 400, want: "F"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := bufferbloatGrade(tt.delta); got != tt.want {
				t.Fatalf("bufferbloatGrade(%.3f) = %q, want %q", tt.delta, got, tt.want)
			}
		})
	}
}

func TestWorseGrade(t *testing.T) {
	tests := []struct {
		name   string
		grades []string
		want   string
	}{
		{name: "empty", grades: []string{}, want: ""},
		{name: "worse direction wins", grades: []string{"A", "D"}, want: "D"},
		{name: "A plus ranks above A", grades: []string{"A+", "A"}, want: "A"},
		{name: "missing phase ignored", grades: []string{"", "B"}, want: "B"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := worseGrade(tt.grades...); got != tt.want {
				t.Fatalf("worseGrade(%v) = %q, want %q", tt.grades, got, tt.want)
			}
		})
	}
}

func TestResponsivenessConfidenceFlagsCPUAndCaps(t *testing.T) {
	result := buildResponsiveness(responsivenessInput{
		profile: SpeedTestProfileQuick,
		baseline: LatencyResult{
			PacketLossValid: true,
			ProbesSent:      10,
			SamplesMS:       []float64{10, 10, 11, 11, 12, 12, 13, 13, 14, 14},
		},
		downloadLoaded: LatencyResult{
			PacketLossValid: true,
			ProbesSent:      10,
			SamplesMS:       []float64{20, 20, 21, 21, 22, 22, 23, 23, 24, 24},
		},
		uploadLoaded: LatencyResult{
			PacketLossValid: true,
			ProbesSent:      10,
			SamplesMS:       []float64{240, 240, 245, 245, 250, 250, 255, 255, 260, 260},
		},
		transfers: responsivenessTransfers{
			download: ThroughputResult{CapHit: true},
		},
		cpuBusyPct: 91,
	})

	if result.OverallGrade != "D" || result.PrimaryDriver != "upload_bufferbloat" {
		t.Fatalf("grading = %+v", result)
	}
	if result.ConfidenceLevel != "low" {
		t.Fatalf("confidence = %q, want low", result.ConfidenceLevel)
	}
	for _, reason := range []string{"agent_cpu_busy_high", "download_phase_cap_hit"} {
		if !slices.Contains(result.ConfidenceReasons, reason) {
			t.Fatalf("confidence reasons %v missing %q", result.ConfidenceReasons, reason)
		}
	}
}

func TestRunSpeedTestStandardMeasuresOptionalPhasesWithinBudgets(t *testing.T) {
	const byteBudget = 300_000
	var pingMu sync.Mutex
	pingCall := 0
	ping := func(ctx context.Context, target string, count uint32) LatencyResult {
		_ = ctx
		_ = target
		pingMu.Lock()
		defer pingMu.Unlock()
		pingCall++
		base := float64(pingCall * 10)
		samples := []float64{base, base + 1, base + 2, base + 3, base + 4}
		return LatencyResult{
			MinMS: base, AvgMS: base + 2, MaxMS: base + 4,
			PacketLossValid: true, ProbesSent: uint32(len(samples)), SamplesMS: samples,
		}
	}
	cpuCall := 0
	result := RunSpeedTest(context.Background(), SpeedTestOptions{
		MaxDurationSeconds:            5,
		MaxDownloadBytes:              byteBudget,
		MaxUploadBytes:                byteBudget,
		PingCount:                     5,
		Profile:                       SpeedTestProfileStandard,
		IncludeBidirectionalInOverall: true,
		EndpointFactory: func(rawURL string) (SpeedEndpoint, error) {
			return &fakeSpeedEndpoint{host: rawURL}, nil
		},
		Ping: ping,
		CPUReader: func() (CPUSample, bool) {
			cpuCall++
			return CPUSample{Busy: uint64(cpuCall * 10), Total: uint64(cpuCall * 100)}, true
		},
	})

	pingMu.Lock()
	gotPingCalls := pingCall
	pingMu.Unlock()
	if gotPingCalls != 5 {
		t.Fatalf("ping calls = %d, want baseline + download + upload + bidirectional + cooldown", gotPingCalls)
	}
	if result.Responsiveness.BidirectionalLoaded.Count == 0 || result.Responsiveness.Cooldown.Count == 0 {
		t.Fatalf("optional phase summaries missing: %+v", result.Responsiveness)
	}
	if !result.Responsiveness.BidirectionalCountsTowardOverall {
		t.Fatal("bidirectional phase should count toward overall")
	}
	primary, bidirectional := splitResponsivenessBudget(byteBudget, SpeedTestProfileStandard)
	if primary+bidirectional != byteBudget {
		t.Fatalf("phase budgets = %d + %d, want hard cap %d", primary, bidirectional, byteBudget)
	}
	if result.Responsiveness.TotalDownloadBytes != byteBudget ||
		result.Responsiveness.TotalUploadBytes != byteBudget {
		t.Fatalf("aggregate bytes exceeded or under-reported cap: %+v", result.Responsiveness)
	}
	if result.Responsiveness.BidirectionalDownload.BytesTransferred != bidirectional ||
		result.Responsiveness.BidirectionalUpload.BytesTransferred != bidirectional {
		t.Fatalf("bidirectional transfer accounting = %+v", result.Responsiveness)
	}
}

func TestQuickProfileCanReportHighConfidence(t *testing.T) {
	baseline := LatencyResult{
		PacketLossValid: true,
		ProbesSent:      10,
		SamplesMS:       []float64{10, 10, 11, 11, 12, 12, 13, 13, 14, 14},
	}
	loadedSamples := make([]float64, loadedPingCount)
	for i := range loadedSamples {
		loadedSamples[i] = 20 + float64(i)
	}
	loaded := LatencyResult{
		PacketLossValid: true,
		ProbesSent:      loadedPingCount,
		SamplesMS:       loadedSamples,
	}
	result := buildResponsiveness(responsivenessInput{
		profile:        SpeedTestProfileQuick,
		baseline:       baseline,
		downloadLoaded: loaded,
		uploadLoaded:   loaded,
	})

	if result.ConfidenceLevel != "high" || len(result.ConfidenceReasons) != 0 {
		t.Fatalf("clean quick confidence = %q %v, want high with no reasons",
			result.ConfidenceLevel, result.ConfidenceReasons)
	}
}

func TestDiagnosticBidirectionalPhaseCannotBecomePrimaryDriver(t *testing.T) {
	baseline := LatencyResult{
		PacketLossValid: true,
		ProbesSent:      10,
		SamplesMS:       []float64{10, 10, 10, 10, 10, 10, 10, 10, 10, 10},
	}
	cleanLoaded := LatencyResult{
		PacketLossValid: true,
		ProbesSent:      10,
		SamplesMS:       []float64{11, 11, 11, 11, 11, 11, 11, 11, 11, 11},
	}
	badBidirectional := LatencyResult{
		PacketLossValid: true,
		ProbesSent:      10,
		SamplesMS:       []float64{500, 500, 500, 500, 500, 500, 500, 500, 500, 500},
	}
	result := buildResponsiveness(responsivenessInput{
		profile:             SpeedTestProfileStandard,
		baseline:            baseline,
		downloadLoaded:      cleanLoaded,
		uploadLoaded:        cleanLoaded,
		bidirectionalLoaded: badBidirectional,
		cooldown:            baseline,
	})

	if result.BidirectionalGrade != "F" || result.OverallGrade != "A+" || result.PrimaryDriver != "none" {
		t.Fatalf("diagnostic bidirectional phase affected primary result: %+v", result)
	}
}

func TestUnknownPacketLossLowersConfidenceWithoutClaimingZeroLoss(t *testing.T) {
	result := buildResponsiveness(responsivenessInput{
		profile: SpeedTestProfileQuick,
		baseline: LatencyResult{
			ProbesSent: 10,
			SamplesMS:  []float64{10, 10, 11, 11, 12, 12, 13, 13, 14, 14},
		},
		downloadLoaded: LatencyResult{ProbesSent: 5, SamplesMS: []float64{20, 21, 22, 23, 24}},
		uploadLoaded:   LatencyResult{ProbesSent: 5, SamplesMS: []float64{20, 21, 22, 23, 24}},
	})

	if result.Baseline.PacketLossValid {
		t.Fatal("packet loss should remain unknown without a ping footer")
	}
	if result.ConfidenceLevel != "medium" ||
		!slices.Contains(result.ConfidenceReasons, "baseline_packet_loss_unknown") {
		t.Fatalf("unknown-loss confidence = %q %v", result.ConfidenceLevel, result.ConfidenceReasons)
	}
}

func TestTruncatedLoadedPhaseIsNotGraded(t *testing.T) {
	// A loaded sampler cut off after one early reply must not produce an
	// authoritative grade: the replies most delayed by queueing are
	// exactly the ones a truncated sampler loses.
	result := buildResponsiveness(responsivenessInput{
		profile: SpeedTestProfileQuick,
		baseline: LatencyResult{
			ProbesSent: 10,
			SamplesMS:  []float64{10, 10, 11, 11, 12, 12, 13, 13, 14, 14},
		},
		downloadLoaded: LatencyResult{ProbesSent: 1, SamplesMS: []float64{10}},
		uploadLoaded: LatencyResult{
			ProbesSent: 5,
			SamplesMS:  []float64{20, 21, 22, 23, 24},
		},
	})

	if result.DownloadGrade != "" || result.DownloadBloatP90MS != 0 {
		t.Fatalf(
			"single-sample loaded phase graded: %q bloat %v",
			result.DownloadGrade,
			result.DownloadBloatP90MS,
		)
	}
	if result.UploadGrade == "" {
		t.Fatalf("adequately sampled phase lost its grade: %+v", result)
	}
	if result.OverallGrade != result.UploadGrade {
		t.Fatalf("overall should rest on graded phases only: %+v", result)
	}
	if !slices.Contains(
		result.ConfidenceReasons,
		"download_loaded_insufficient_samples",
	) {
		t.Fatalf(
			"missing insufficient-samples reason: %v",
			result.ConfidenceReasons,
		)
	}
}
