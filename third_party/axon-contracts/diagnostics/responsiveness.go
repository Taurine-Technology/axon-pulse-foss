package diagnostics

import (
	"math"
	"slices"
	"strings"
)

const (
	SpeedTestProfileQuick    SpeedTestProfile = "quick"
	SpeedTestProfileStandard SpeedTestProfile = "standard"
	SpeedTestProfileScenario SpeedTestProfile = "scenario"

	minimumReliablePercentileSamples = 10
	highCPUBusyPct                   = 85.0

	// A loaded phase that ended after one or two early replies would grade
	// exactly the traffic the load had not yet delayed — the replies most
	// delayed by queueing are the ones a truncated sampler loses. Below
	// this floor no bloat delta or grade is derived; the phase surfaces
	// through the *_insufficient_samples confidence reason instead. Three
	// also matches the legacy min/avg/max compatibility summaries.
	minimumGradeableSamples = 3
)

type (
	// SpeedTestProfile selects the bounded set of responsiveness phases.
	SpeedTestProfile string

	// LatencySummary is a bounded distribution for one measurement phase.
	LatencySummary struct {
		Count             uint32
		MinMS             float64
		P5MS              float64
		P50MS             float64
		P90MS             float64
		P95MS             float64
		P99MS             float64
		MaxMS             float64
		JitterIQRMS       float64
		JitterMeanDeltaMS float64
		PacketLossPct     float64
		PacketLossValid   bool
		Error             string
	}

	// ResponsivenessResult contains phase summaries and bufferbloat grades.
	ResponsivenessResult struct {
		Baseline            LatencySummary
		DownloadLoaded      LatencySummary
		UploadLoaded        LatencySummary
		BidirectionalLoaded LatencySummary
		Cooldown            LatencySummary

		DownloadBloatP90MS      float64
		UploadBloatP90MS        float64
		BidirectionalBloatP90MS float64

		DownloadGrade      string
		UploadGrade        string
		BidirectionalGrade string
		OverallGrade       string
		PrimaryDriver      string

		ConfidenceLevel   string
		ConfidenceReasons []string

		Profile                          string
		BidirectionalCountsTowardOverall bool
		BidirectionalDownload            ThroughputResult
		BidirectionalUpload              ThroughputResult
		TotalDownloadBytes               uint64
		TotalUploadBytes                 uint64
	}

	responsivenessTransfers struct {
		download              ThroughputResult
		upload                ThroughputResult
		bidirectionalDownload ThroughputResult
		bidirectionalUpload   ThroughputResult
	}

	responsivenessInput struct {
		profile              SpeedTestProfile
		includeBidirectional bool
		baseline             LatencyResult
		downloadLoaded       LatencyResult
		uploadLoaded         LatencyResult
		bidirectionalLoaded  LatencyResult
		cooldown             LatencyResult
		transfers            responsivenessTransfers
		cpuBusyPct           float64
	}
)

func normalizeSpeedTestProfile(profile SpeedTestProfile) SpeedTestProfile {
	switch profile {
	case SpeedTestProfileStandard, SpeedTestProfileScenario:
		return profile
	default:
		return SpeedTestProfileQuick
	}
}

func latencySummary(result LatencyResult) LatencySummary {
	samples := append([]float64{}, result.SamplesMS...)
	if len(samples) == 0 && result.Error == "" && result.ProbesSent > 0 {
		// Compatibility for injected/legacy ping implementations that only
		// return the original aggregate fields. RunPing itself always supplies
		// bounded samples.
		samples = []float64{result.MinMS, result.AvgMS, result.MaxMS}
	}

	summary := LatencySummary{
		Count:             uint32(len(samples)),
		PacketLossPct:     roundTo(result.PacketLossPct, 3),
		PacketLossValid:   result.PacketLossValid,
		JitterMeanDeltaMS: roundTo(computeJitter(samples), 3),
		Error:             result.Error,
	}
	if len(samples) == 0 {
		return summary
	}

	sorted := append([]float64{}, samples...)
	slices.Sort(sorted)
	summary.MinMS = roundTo(sorted[0], 3)
	summary.P5MS = roundTo(percentileSorted(sorted, 5), 3)
	summary.P50MS = roundTo(percentileSorted(sorted, 50), 3)
	summary.P90MS = roundTo(percentileSorted(sorted, 90), 3)
	summary.P95MS = roundTo(percentileSorted(sorted, 95), 3)
	summary.P99MS = roundTo(percentileSorted(sorted, 99), 3)
	summary.MaxMS = roundTo(sorted[len(sorted)-1], 3)
	summary.JitterIQRMS = roundTo(
		percentileSorted(sorted, 75)-percentileSorted(sorted, 25),
		3,
	)
	return summary
}

func percentile(values []float64, pct float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64{}, values...)
	slices.Sort(sorted)
	return percentileSorted(sorted, pct)
}

func percentileSorted(sorted []float64, pct float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if pct <= 0 {
		return sorted[0]
	}
	if pct >= 100 {
		return sorted[len(sorted)-1]
	}

	position := pct / 100 * float64(len(sorted)-1)
	lower := int(math.Floor(position))
	upper := int(math.Ceil(position))
	if lower == upper {
		return sorted[lower]
	}
	fraction := position - float64(lower)
	return sorted[lower] + fraction*(sorted[upper]-sorted[lower])
}

func bufferbloatGrade(deltaMS float64) string {
	switch {
	case deltaMS < 5:
		return "A+"
	case deltaMS < 30:
		return "A"
	case deltaMS < 60:
		return "B"
	case deltaMS < 200:
		return "C"
	case deltaMS < 400:
		return "D"
	default:
		return "F"
	}
}

func worseGrade(grades ...string) string {
	ranks := map[string]int{
		"A+": 0,
		"A":  1,
		"B":  2,
		"C":  3,
		"D":  4,
		"F":  5,
	}
	worst := ""
	worstRank := -1
	for _, grade := range grades {
		rank, ok := ranks[grade]
		if ok && rank > worstRank {
			worst = grade
			worstRank = rank
		}
	}
	return worst
}

func bloatAgainstBaseline(baseline, loaded LatencySummary) (float64, string) {
	if baseline.Count < minimumGradeableSamples || loaded.Count < minimumGradeableSamples {
		return 0, ""
	}
	delta := roundTo(max(loaded.P90MS-baseline.P5MS, 0), 3)
	return delta, bufferbloatGrade(delta)
}

func buildResponsiveness(input responsivenessInput) ResponsivenessResult {
	profile := normalizeSpeedTestProfile(input.profile)
	result := ResponsivenessResult{
		Baseline:                         latencySummary(input.baseline),
		DownloadLoaded:                   latencySummary(input.downloadLoaded),
		UploadLoaded:                     latencySummary(input.uploadLoaded),
		BidirectionalLoaded:              latencySummary(input.bidirectionalLoaded),
		Cooldown:                         latencySummary(input.cooldown),
		ConfidenceReasons:                []string{},
		Profile:                          string(profile),
		BidirectionalCountsTowardOverall: input.includeBidirectional,
		BidirectionalDownload:            input.transfers.bidirectionalDownload,
		BidirectionalUpload:              input.transfers.bidirectionalUpload,
		TotalDownloadBytes: input.transfers.download.BytesTransferred +
			input.transfers.bidirectionalDownload.BytesTransferred,
		TotalUploadBytes: input.transfers.upload.BytesTransferred +
			input.transfers.bidirectionalUpload.BytesTransferred,
	}

	result.DownloadBloatP90MS, result.DownloadGrade = bloatAgainstBaseline(
		result.Baseline,
		result.DownloadLoaded,
	)
	result.UploadBloatP90MS, result.UploadGrade = bloatAgainstBaseline(
		result.Baseline,
		result.UploadLoaded,
	)
	result.BidirectionalBloatP90MS, result.BidirectionalGrade = bloatAgainstBaseline(
		result.Baseline,
		result.BidirectionalLoaded,
	)
	result.OverallGrade = worseGrade(result.DownloadGrade, result.UploadGrade)
	if input.includeBidirectional {
		result.OverallGrade = worseGrade(result.OverallGrade, result.BidirectionalGrade)
	}
	result.PrimaryDriver = primaryResponsivenessDriver(result)
	result.ConfidenceLevel, result.ConfidenceReasons = responsivenessConfidence(
		result,
		input.transfers,
		input.cpuBusyPct,
	)
	return result
}

func primaryResponsivenessDriver(result ResponsivenessResult) string {
	if result.Baseline.Count == 0 {
		return "latency_unavailable"
	}
	maxLoss := max(
		validPacketLoss(result.Baseline),
		validPacketLoss(result.DownloadLoaded),
		validPacketLoss(result.UploadLoaded),
	)
	maxBloat := max(result.DownloadBloatP90MS, result.UploadBloatP90MS)
	if result.BidirectionalCountsTowardOverall {
		maxLoss = max(maxLoss, validPacketLoss(result.BidirectionalLoaded))
		maxBloat = max(maxBloat, result.BidirectionalBloatP90MS)
	}
	if maxLoss >= 2 && maxBloat < 30 {
		return "packet_loss"
	}
	if maxBloat < 5 {
		return "none"
	}

	if result.UploadGrade != "" && result.UploadBloatP90MS == maxBloat {
		return "upload_bufferbloat"
	}
	if result.DownloadGrade != "" && result.DownloadBloatP90MS == maxBloat {
		return "download_bufferbloat"
	}
	if result.BidirectionalCountsTowardOverall &&
		result.BidirectionalGrade != "" &&
		result.BidirectionalBloatP90MS == maxBloat {
		return "bidirectional_bufferbloat"
	}
	return "none"
}

func validPacketLoss(summary LatencySummary) float64 {
	if !summary.PacketLossValid {
		return 0
	}
	return summary.PacketLossPct
}

func responsivenessConfidence(
	result ResponsivenessResult,
	transfers responsivenessTransfers,
	cpuBusyPct float64,
) (string, []string) {
	reasons := []string{}
	severity := 0
	add := func(reason string, reasonSeverity int) {
		if reason == "" || slices.Contains(reasons, reason) {
			return
		}
		reasons = append(reasons, reason)
		severity = max(severity, reasonSeverity)
	}

	if cpuBusyPct >= highCPUBusyPct {
		add("agent_cpu_busy_high", 2)
	}
	addTransferConfidence(&reasons, &severity, "download", transfers.download)
	addTransferConfidence(&reasons, &severity, "upload", transfers.upload)
	addTransferConfidence(&reasons, &severity, "bidirectional_download", transfers.bidirectionalDownload)
	addTransferConfidence(&reasons, &severity, "bidirectional_upload", transfers.bidirectionalUpload)

	phaseSummaries := []struct {
		name    string
		summary LatencySummary
	}{
		{name: "baseline", summary: result.Baseline},
		{name: "download_loaded", summary: result.DownloadLoaded},
		{name: "upload_loaded", summary: result.UploadLoaded},
	}
	if result.Profile != string(SpeedTestProfileQuick) {
		phaseSummaries = append(
			phaseSummaries,
			struct {
				name    string
				summary LatencySummary
			}{name: "bidirectional_loaded", summary: result.BidirectionalLoaded},
			struct {
				name    string
				summary LatencySummary
			}{name: "cooldown", summary: result.Cooldown},
		)
	}
	for _, phase := range phaseSummaries {
		if strings.Contains(phase.summary.Error, "ping binary not found") {
			add("ping_binary_unavailable", 2)
		}
		if phase.summary.Error != "" {
			add(phase.name+"_latency_error", 2)
			continue
		}
		if phase.summary.Count == 0 {
			add(phase.name+"_latency_missing", 2)
			continue
		}
		if phase.summary.Count < minimumReliableSamples(result.Profile, phase.name) {
			add(phase.name+"_insufficient_samples", 1)
		}
		switch {
		case !phase.summary.PacketLossValid:
			add(phase.name+"_packet_loss_unknown", 1)
		case phase.summary.PacketLossPct >= 20:
			add(phase.name+"_packet_loss_high", 2)
		case phase.summary.PacketLossPct > 0:
			add(phase.name+"_packet_loss", 1)
		}
	}

	level := "high"
	if severity == 1 {
		level = "medium"
	} else if severity >= 2 {
		level = "low"
	}
	return level, reasons
}

func minimumReliableSamples(profile, phase string) uint32 {
	if profile == string(SpeedTestProfileQuick) && phase != "baseline" {
		return loadedPingCount
	}
	return minimumReliablePercentileSamples
}

func addTransferConfidence(reasons *[]string, severity *int, phase string, result ThroughputResult) {
	add := func(reason string, reasonSeverity int) {
		if reason == "" || slices.Contains(*reasons, reason) {
			return
		}
		*reasons = append(*reasons, reason)
		*severity = max(*severity, reasonSeverity)
	}
	if result.CapHit {
		add(phase+"_phase_cap_hit", 1)
	}
	if result.Error != "" {
		severity := 2
		if strings.HasPrefix(result.Error, "partial:") {
			severity = 1
		}
		add(phase+"_endpoint_error", severity)
	}
}
