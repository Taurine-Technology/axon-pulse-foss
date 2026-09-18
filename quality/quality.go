// Package quality contains controller-independent Internet quality policy.
//
// It deliberately has no Axon, persistence, UI, or transport dependencies so
// the measurement policy can be extracted into the future FOSS Pulse core.
package quality

import (
	"math"
	"slices"
)

type (
	Rating string

	// Budget is the hard, locally enforced envelope for one test. Requested
	// values are retained so results explain when controller or UI policy was
	// clamped by the sensor.
	Budget struct {
		RequestedDownloadBytes uint64 `json:"requested_download_bytes"`
		RequestedUploadBytes   uint64 `json:"requested_upload_bytes"`
		EffectiveDownloadBytes uint64 `json:"effective_download_bytes"`
		EffectiveUploadBytes   uint64 `json:"effective_upload_bytes"`
		MaxDurationSeconds     int    `json:"max_duration_seconds"`
		MaxConcurrentRequests  int    `json:"max_concurrent_requests"`
		PredictedBytes         uint64 `json:"predicted_bytes"`
		UsedBytes              uint64 `json:"used_bytes"`
		CapHit                 bool   `json:"cap_hit"`
	}

	// Bucket is a one-second (or similar) slice of a scored transfer phase.
	// Calibration, ramp-up, stream-count changes, and recovery use separate
	// buckets and are never silently mixed into the steady-state result.
	Bucket struct {
		Index          int     `json:"index"`
		StartMS        int64   `json:"start_ms"`
		DurationMS     int64   `json:"duration_ms"`
		Bytes          uint64  `json:"bytes"`
		Samples        int     `json:"samples"`
		StreamCount    int     `json:"stream_count"`
		Complete       bool    `json:"complete"`
		ThroughputMbps float64 `json:"throughput_mbps"`
	}

	StabilityPolicy struct {
		MinimumBucketDurationMS int64
		MinimumSamples          int
		RequiredBuckets         int
		MaxCoefficientVariation float64
		MaxMADRatio             float64
	}

	StableRegion struct {
		StartBucket          int     `json:"start_bucket"`
		EndBucket            int     `json:"end_bucket"`
		BucketCount          int     `json:"bucket_count"`
		Stable               bool    `json:"stable"`
		ThroughputMbps       float64 `json:"throughput_mbps"`
		CoefficientVariation float64 `json:"coefficient_variation"`
		MADRatio             float64 `json:"mad_ratio"`
		Reason               string  `json:"reason,omitempty"`
	}

	PhaseMetrics struct {
		P95LatencyMS   float64 `json:"p95_latency_ms"`
		LossPct        float64 `json:"loss_pct"`
		LatencySamples int     `json:"latency_samples"`
		LossAvailable  bool    `json:"loss_available"`
	}

	ApplicationInput struct {
		Baseline          PhaseMetrics `json:"baseline"`
		Download          PhaseMetrics `json:"download"`
		Upload            PhaseMetrics `json:"upload"`
		Bidirectional     PhaseMetrics `json:"bidirectional"`
		DownloadP75Mbps   float64      `json:"download_p75_mbps"`
		UploadP75Mbps     float64      `json:"upload_p75_mbps"`
		DownloadAvailable bool         `json:"download_available"`
		UploadAvailable   bool         `json:"upload_available"`
	}

	ApplicationScore struct {
		Available          bool     `json:"available"`
		Name               string   `json:"name"`
		Score              float64  `json:"score"`
		Rating             Rating   `json:"rating"`
		LatencyScore       float64  `json:"latency_score"`
		LossScore          float64  `json:"loss_score"`
		ThroughputScore    float64  `json:"throughput_score"`
		MeasuredLatencyMS  float64  `json:"measured_latency_ms"`
		MeasuredLossPct    float64  `json:"measured_loss_pct"`
		DownloadMbps       float64  `json:"download_mbps"`
		UploadMbps         float64  `json:"upload_mbps"`
		Explanation        string   `json:"explanation"`
		UnavailableReasons []string `json:"unavailable_reasons,omitempty"`
	}

	Experience struct {
		Available    bool                        `json:"available"`
		OverallScore float64                     `json:"overall_score"`
		Rating       Rating                      `json:"rating"`
		Home         map[string]ApplicationScore `json:"home"`
		Additional   map[string]ApplicationScore `json:"additional,omitempty"`
	}

	appProfile struct {
		key, name, phase, explanation                               string
		latencyGood, latencyBad                                     float64
		lossGood, lossBad                                           float64
		downloadGood, downloadMin                                   float64
		uploadGood, uploadMin                                       float64
		latencyWeight, lossWeight, throughputWeight, floorInfluence float64
	}

	ConfidenceInput struct {
		BaselineSamples       int             `json:"baseline_samples"`
		PhaseSamples          map[string]int  `json:"phase_samples"`
		StablePhases          map[string]bool `json:"stable_phases"`
		ProbeSuccessRatio     float64         `json:"probe_success_ratio"`
		FreshPathSuccessRatio float64         `json:"fresh_path_success_ratio"`
		AgentCPUBusyPct       float64         `json:"agent_cpu_busy_pct"`
		EndpointCV            float64         `json:"endpoint_cv"`
		SourceDisagreementPct float64         `json:"source_disagreement_pct"`
		NetworkChanged        bool            `json:"network_changed"`
		IPFamilyChanged       bool            `json:"ip_family_changed"`
	}

	Confidence struct {
		Level   string   `json:"level"`
		Reasons []string `json:"reasons"`
	}
)

const (
	// MinimumBaselineSamples prevents a single slow request in a short idle
	// phase from dominating the p95 used for the browsing verdict.
	MinimumBaselineSamples = 20

	// MinimumLoadedSamples is the least loaded-latency evidence a
	// phase-specific outcome may be scored from. A byte-budget-capped phase
	// on a fast link can end with two or three probes; a confident "bad"
	// verdict from that little data is worse than an honest "unavailable".
	MinimumLoadedSamples = 5

	RatingGood        Rating = "good"
	RatingAverage     Rating = "average"
	RatingBad         Rating = "bad"
	RatingUnavailable Rating = "unavailable"
)

var (
	applicationProfiles = []appProfile{
		{key: "browsing", name: "Browsing", phase: "baseline", explanation: "Starting pages and requests should feel responsive.", latencyGood: 60, latencyBad: 180, lossGood: .5, lossBad: 2.5, downloadGood: 20, downloadMin: 2, uploadGood: 3, uploadMin: .5, latencyWeight: .55, lossWeight: .20, throughputWeight: .25, floorInfluence: .20},
		{key: "streaming", name: "Streaming", phase: "download", explanation: "Sustained video should have enough headroom without stalls.", latencyGood: 90, latencyBad: 260, lossGood: 1, lossBad: 4, downloadGood: 25, downloadMin: 5, uploadGood: 1.5, uploadMin: .3, latencyWeight: .15, lossWeight: .10, throughputWeight: .75, floorInfluence: .15},
		{key: "video_calls", name: "Video calls", phase: "bidirectional", explanation: "Conversation should stay clear while sending and receiving together.", latencyGood: 70, latencyBad: 200, lossGood: .8, lossBad: 3, downloadGood: 4, downloadMin: 1.5, uploadGood: 4, uploadMin: 1.5, latencyWeight: .45, lossWeight: .35, throughputWeight: .20, floorInfluence: .35},
		{key: "gaming", name: "Gaming", phase: "bidirectional", explanation: "Controls should remain responsive while the connection is busy.", latencyGood: 55, latencyBad: 140, lossGood: .5, lossBad: 2, downloadGood: 8, downloadMin: 1.5, uploadGood: 3, uploadMin: .5, latencyWeight: .60, lossWeight: .30, throughputWeight: .10, floorInfluence: .35},
		{key: "audio_calls", name: "Audio calls", phase: "bidirectional", explanation: "Voice needs little bandwidth but is sensitive to delay and loss.", latencyGood: 90, latencyBad: 250, lossGood: 1, lossBad: 4, downloadGood: .5, downloadMin: .1, uploadGood: .5, uploadMin: .1, latencyWeight: .50, lossWeight: .35, throughputWeight: .15, floorInfluence: .25},
		{key: "backup", name: "Backup", phase: "upload", explanation: "Large uploads depend mainly on sustained upstream throughput.", latencyGood: 180, latencyBad: 400, lossGood: 2, lossBad: 6, downloadGood: 2, downloadMin: .5, uploadGood: 15, uploadMin: 2, latencyWeight: .10, lossWeight: .10, throughputWeight: .80, floorInfluence: .20},
	}
)

func DefaultStabilityPolicy() StabilityPolicy {
	return StabilityPolicy{
		MinimumBucketDurationMS: 900,
		MinimumSamples:          1,
		RequiredBuckets:         3,
		MaxCoefficientVariation: 0.10,
		MaxMADRatio:             0.08,
	}
}

// DetectStableRegion finds the first complete, unchanged-stream window whose
// robust spread is small enough. It then extends the region through later
// compatible buckets. The caller may end a phase early as soon as Stable is
// true, while an unstable phase continues toward its maximum time or byte cap.
func DetectStableRegion(buckets []Bucket, policy StabilityPolicy) StableRegion {
	policy = normalizeStabilityPolicy(policy)
	usable := make([]Bucket, 0, len(buckets))
	for _, bucket := range buckets {
		if !bucket.Complete || bucket.DurationMS < policy.MinimumBucketDurationMS || bucket.Samples < policy.MinimumSamples || bucket.Bytes == 0 {
			continue
		}
		bucket.ThroughputMbps = bucketMbps(bucket)
		if finitePositive(bucket.ThroughputMbps) {
			usable = append(usable, bucket)
		}
	}
	if len(usable) < policy.RequiredBuckets {
		return StableRegion{Reason: "insufficient_complete_buckets", BucketCount: len(usable)}
	}

	for start := 0; start+policy.RequiredBuckets <= len(usable); start++ {
		window := usable[start : start+policy.RequiredBuckets]
		if !consecutiveWithSameStreams(window) {
			continue
		}
		values := bucketValues(window)
		cv, madRatio, center := robustSpread(values)
		if cv > policy.MaxCoefficientVariation || madRatio > policy.MaxMADRatio {
			continue
		}
		end := start + policy.RequiredBuckets
		for end < len(usable) {
			candidate := usable[end]
			previous := usable[end-1]
			if candidate.Index != previous.Index+1 || candidate.StreamCount != previous.StreamCount {
				break
			}
			allowed := max(0.15*center, 2*medianAbsoluteDeviation(values))
			if math.Abs(candidate.ThroughputMbps-center) > allowed {
				break
			}
			values = append(values, candidate.ThroughputMbps)
			end++
		}
		cv, madRatio, center = robustSpread(values)
		return StableRegion{
			StartBucket: usable[start].Index, EndBucket: usable[end-1].Index,
			BucketCount: len(values), Stable: true, ThroughputMbps: round(center, 3),
			CoefficientVariation: round(cv, 4), MADRatio: round(madRatio, 4),
		}
	}

	values := bucketValues(usable)
	cv, madRatio, center := robustSpread(values)
	return StableRegion{
		StartBucket: usable[0].Index, EndBucket: usable[len(usable)-1].Index,
		BucketCount: len(usable), ThroughputMbps: round(center, 3),
		CoefficientVariation: round(cv, 4), MADRatio: round(madRatio, 4),
		Reason: "throughput_did_not_stabilize",
	}
}

// DetectBidirectionalStableRegion requires each direction to be stable on
// its own and both stable regions to overlap for at least the policy's
// required bucket count. A stable sum of the two directions is not evidence:
// opposing changes cancel out and one direction can collapse unnoticed. The
// reported throughput is the sum of the two per-direction medians and the
// bucket range is the overlap.
func DetectBidirectionalStableRegion(download, upload []Bucket, policy StabilityPolicy) StableRegion {
	policy = normalizeStabilityPolicy(policy)
	down := DetectStableRegion(download, policy)
	up := DetectStableRegion(upload, policy)
	region := StableRegion{ThroughputMbps: round(down.ThroughputMbps+up.ThroughputMbps, 3)}
	switch {
	case !down.Stable && !up.Stable:
		region.Reason = "neither_direction_stable"
		if down.Reason == "insufficient_complete_buckets" || up.Reason == "insufficient_complete_buckets" {
			region.Reason = "insufficient_complete_buckets"
		}
		region.BucketCount = min(down.BucketCount, up.BucketCount)
		return region
	case !down.Stable:
		// Missing complete buckets is a budget or time property, not
		// instability; keep the two causes apart for the consumer.
		region.Reason = "download_not_stable"
		if down.Reason == "insufficient_complete_buckets" {
			region.Reason = down.Reason
		}
		region.BucketCount = down.BucketCount
		return region
	case !up.Stable:
		region.Reason = "upload_not_stable"
		if up.Reason == "insufficient_complete_buckets" {
			region.Reason = up.Reason
		}
		region.BucketCount = up.BucketCount
		return region
	}
	start := max(down.StartBucket, up.StartBucket)
	end := min(down.EndBucket, up.EndBucket)
	overlap := end - start + 1
	if overlap < policy.RequiredBuckets {
		region.Reason = "directions_not_overlapping"
		region.BucketCount = max(overlap, 0)
		return region
	}
	region.Stable = true
	region.StartBucket, region.EndBucket, region.BucketCount = start, end, overlap
	region.CoefficientVariation = round(max(down.CoefficientVariation, up.CoefficientVariation), 4)
	region.MADRatio = round(max(down.MADRatio, up.MADRatio), 4)
	return region
}

// ScoreApplications deliberately keeps the four home-facing outcomes separate
// from the diagnostic audio/backup profiles. A poor outcome is never treated
// as low measurement confidence.
func ScoreApplications(input ApplicationInput) Experience {
	home := map[string]ApplicationScore{}
	additional := map[string]ApplicationScore{}
	var homeTotal float64
	homeAvailable := 0
	for _, profile := range applicationProfiles {
		score := scoreApplication(input, profile)
		if profile.key == "audio_calls" || profile.key == "backup" {
			additional[profile.key] = score
			continue
		}
		home[profile.key] = score
		if score.Available {
			homeTotal += score.Score
			homeAvailable++
		}
	}
	if homeAvailable == 0 {
		return Experience{Rating: RatingUnavailable, Home: home, Additional: additional}
	}
	overall := round(homeTotal/float64(homeAvailable), 1)
	return Experience{Available: true, OverallScore: overall, Rating: rating(overall), Home: home, Additional: additional}
}

func GradeConfidence(input ConfidenceInput) Confidence {
	reasons := []string{}
	severity := 0
	add := func(reason string, level int) {
		if reason == "" || slices.Contains(reasons, reason) {
			return
		}
		reasons = append(reasons, reason)
		severity = max(severity, level)
	}
	if input.BaselineSamples < MinimumBaselineSamples {
		add("baseline_insufficient_samples", 1)
	}
	for _, phase := range []string{"download", "upload", "bidirectional"} {
		if input.PhaseSamples[phase] < 10 {
			add(phase+"_insufficient_samples", 1)
		}
		if !input.StablePhases[phase] {
			add(phase+"_did_not_stabilize", 1)
		}
	}
	if input.ProbeSuccessRatio < .7 {
		add("too_few_latency_probes", 2)
	} else if input.ProbeSuccessRatio < .9 {
		add("some_latency_probes_missing", 1)
	}
	if input.FreshPathSuccessRatio > 0 && input.FreshPathSuccessRatio < .7 {
		add("fresh_path_probes_unreliable", 1)
	}
	if input.AgentCPUBusyPct >= 85 {
		add("agent_cpu_busy_high", 2)
	}
	if input.EndpointCV > .25 {
		add("traffic_endpoints_inconsistent", 1)
	}
	if input.SourceDisagreementPct > 25 {
		add("measurement_sources_disagree", 1)
	}
	if input.NetworkChanged {
		add("network_changed_during_test", 2)
	}
	if input.IPFamilyChanged {
		add("ip_family_changed_during_test", 2)
	}
	level := "high"
	if severity == 1 {
		level = "medium"
	} else if severity >= 2 {
		level = "low"
	}
	return Confidence{Level: level, Reasons: reasons}
}

func scoreApplication(input ApplicationInput, profile appProfile) ApplicationScore {
	phase := input.Baseline
	starvedReason := ""
	switch profile.phase {
	case "download":
		phase = input.Download
	case "upload":
		phase = input.Upload
	case "bidirectional":
		phase = input.Bidirectional
	}
	if profile.phase == "baseline" && phase.LatencySamples < MinimumBaselineSamples {
		starvedReason = "insufficient_baseline_samples"
	}
	if profile.phase != "baseline" && phase.LatencySamples < MinimumLoadedSamples {
		starvedReason = "insufficient_load_samples"
	}
	if starvedReason != "" {
		phase = PhaseMetrics{}
	}
	result := ApplicationScore{
		Name: profile.name, Rating: RatingUnavailable,
		MeasuredLatencyMS: round(phase.P95LatencyMS, 1), MeasuredLossPct: round(phase.LossPct, 2),
		DownloadMbps: round(input.DownloadP75Mbps, 1), UploadMbps: round(input.UploadP75Mbps, 1),
		Explanation: profile.explanation,
	}
	if starvedReason != "" {
		result.UnavailableReasons = append(result.UnavailableReasons, starvedReason)
	}
	var weighted, availableWeight float64
	if phase.LatencySamples > 0 {
		result.LatencyScore = round(lowerIsBetter(phase.P95LatencyMS, profile.latencyGood, profile.latencyBad), 1)
		weighted += profile.latencyWeight * result.LatencyScore
		availableWeight += profile.latencyWeight
	} else {
		result.UnavailableReasons = append(result.UnavailableReasons, "latency_unavailable")
	}
	if phase.LossAvailable {
		result.LossScore = round(lowerIsBetter(phase.LossPct, profile.lossGood, profile.lossBad), 1)
		weighted += profile.lossWeight * result.LossScore
		availableWeight += profile.lossWeight
	} else {
		result.UnavailableReasons = append(result.UnavailableReasons, "loss_unavailable")
	}
	throughputAvailable := input.DownloadAvailable && input.UploadAvailable
	if throughputAvailable {
		down := higherIsBetter(input.DownloadP75Mbps, profile.downloadMin, profile.downloadGood)
		up := higherIsBetter(input.UploadP75Mbps, profile.uploadMin, profile.uploadGood)
		result.ThroughputScore = round(min(down, up), 1)
		weighted += profile.throughputWeight * result.ThroughputScore
		availableWeight += profile.throughputWeight
	} else {
		result.UnavailableReasons = append(result.UnavailableReasons, "throughput_unavailable")
	}
	if profile.throughputWeight > 0 && !throughputAvailable {
		return result
	}
	// A phase-specific application outcome needs at least one phase observation;
	// throughput alone must not make missing latency/loss look healthy.
	if phase.LatencySamples == 0 && !phase.LossAvailable {
		return result
	}
	if availableWeight == 0 {
		return result
	}
	base := weighted / availableWeight
	ratio := min(clamp01(input.DownloadP75Mbps/profile.downloadMin), clamp01(input.UploadP75Mbps/profile.uploadMin))
	base *= 1 - profile.floorInfluence + profile.floorInfluence*ratio
	final := round(clamp(base, 0, 100), 1)
	result.Available = true
	result.Score = final
	result.Rating = rating(final)
	return result
}

func lowerIsBetter(measured, good, bad float64) float64 {
	if bad <= good {
		return 0
	}
	return clamp(100*(1-(measured-good)/(bad-good)), 0, 100)
}

func higherIsBetter(measured, minimum, good float64) float64 {
	if good <= minimum {
		return 0
	}
	return clamp(100*(measured-minimum)/(good-minimum), 0, 100)
}

func rating(score float64) Rating {
	if score >= 80 {
		return RatingGood
	}
	if score >= 60 {
		return RatingAverage
	}
	return RatingBad
}

func normalizeStabilityPolicy(policy StabilityPolicy) StabilityPolicy {
	defaults := DefaultStabilityPolicy()
	if policy.MinimumBucketDurationMS <= 0 {
		policy.MinimumBucketDurationMS = defaults.MinimumBucketDurationMS
	}
	if policy.MinimumSamples <= 0 {
		policy.MinimumSamples = defaults.MinimumSamples
	}
	if policy.RequiredBuckets < 2 {
		policy.RequiredBuckets = defaults.RequiredBuckets
	}
	if policy.MaxCoefficientVariation <= 0 || policy.MaxCoefficientVariation > 1 {
		policy.MaxCoefficientVariation = defaults.MaxCoefficientVariation
	}
	if policy.MaxMADRatio <= 0 || policy.MaxMADRatio > 1 {
		policy.MaxMADRatio = defaults.MaxMADRatio
	}
	return policy
}

func bucketMbps(bucket Bucket) float64 {
	if bucket.DurationMS <= 0 {
		return 0
	}
	return float64(bucket.Bytes) * 8 / float64(bucket.DurationMS) / 1000
}

func bucketValues(buckets []Bucket) []float64 {
	values := make([]float64, len(buckets))
	for index, bucket := range buckets {
		values[index] = bucket.ThroughputMbps
	}
	return values
}

func consecutiveWithSameStreams(buckets []Bucket) bool {
	for index := 1; index < len(buckets); index++ {
		if buckets[index].Index != buckets[index-1].Index+1 || buckets[index].StreamCount != buckets[index-1].StreamCount {
			return false
		}
	}
	return true
}

func robustSpread(values []float64) (float64, float64, float64) {
	if len(values) == 0 {
		return 0, 0, 0
	}
	center := median(values)
	mean := 0.0
	for _, value := range values {
		mean += value
	}
	mean /= float64(len(values))
	variance := 0.0
	for _, value := range values {
		variance += (value - mean) * (value - mean)
	}
	variance /= float64(len(values))
	cv := 0.0
	if mean > 0 {
		cv = math.Sqrt(variance) / mean
	}
	madRatio := 0.0
	if center > 0 {
		madRatio = medianAbsoluteDeviation(values) / center
	}
	return cv, madRatio, center
}

func medianAbsoluteDeviation(values []float64) float64 {
	center := median(values)
	deviations := make([]float64, len(values))
	for index, value := range values {
		deviations[index] = math.Abs(value - center)
	}
	return median(deviations)
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	ordered := append([]float64(nil), values...)
	slices.Sort(ordered)
	middle := len(ordered) / 2
	if len(ordered)%2 == 0 {
		return (ordered[middle-1] + ordered[middle]) / 2
	}
	return ordered[middle]
}

func finitePositive(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func clamp(value, low, high float64) float64 { return min(max(value, low), high) }
func clamp01(value float64) float64          { return clamp(value, 0, 1) }
func round(value float64, places int) float64 {
	factor := math.Pow10(places)
	return math.Round(value*factor) / factor
}
