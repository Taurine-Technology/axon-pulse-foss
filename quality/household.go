package quality

import (
	"fmt"
	"math"
	"slices"
	"sort"
)

// Household scenarios reproduce a configured mix of everyday activities,
// each paced to its own reference demand, and score every activity from its
// own delivery requirements. Nothing here saturates the link; saturation
// evidence lives in the stress profiles and never decides a household status.

const (
	// HouseholdScenarioVersion identifies the activity catalog and pacing
	// model. Results from different scenario versions are not comparable.
	HouseholdScenarioVersion = 1
	// HouseholdScorerVersion identifies the scoring policy applied to the
	// activity measurements. Version 2 judges real-time activities on p90
	// delay, robust jitter and the missed-deadline share instead of p95.
	HouseholdScorerVersion = 2
	// HouseholdTransportHTTPS is the only transport this build supports.
	// Game and call loss over HTTPS is an estimate, never UDP media quality.
	HouseholdTransportHTTPS = "https"

	// HouseholdDefaultDownloadCap and HouseholdDefaultUploadCap sum to the
	// 300 MB (decimal) per-test allowance the product promises.
	HouseholdDefaultDownloadCap uint64 = 225_000_000
	HouseholdDefaultUploadCap   uint64 = 75_000_000
	// HouseholdOverheadReserve is the share of the cap kept back for protocol
	// overhead the payload ledger cannot see (headers, TLS, retransmission).
	HouseholdOverheadReserve = 0.10

	// MaxHouseholdInstances bounds concurrent activity instances so the
	// sensor's connection count and memory stay predictable.
	MaxHouseholdInstances = 8
	maxHouseholdCount     = 4

	MixSourceDefault    = "default"
	MixSourceController = "controller"
	MixSourceLocal      = "local"

	HouseholdStatusPass         = "pass"
	HouseholdStatusStruggle     = "struggle"
	HouseholdStatusFail         = "fail"
	HouseholdStatusInsufficient = "insufficient"

	StopEnoughEvidence    = "enough_evidence"
	StopInsufficientBytes = "insufficient_bytes"
	StopInsufficientTime  = "insufficient_time"
	StopTransportFailure  = "transport_failure"
	StopCancelled         = "cancelled"

	PatternSegments = "segments"
	PatternFrames   = "frames"
	PatternProbes   = "probes"
	PatternBursts   = "bursts"

	AppStreaming  = "streaming"
	AppVideoCalls = "video_calls"
	AppGaming     = "gaming"
	AppBrowsing   = "browsing"

	// householdReferenceSegments is how many segments the lone reference
	// stream fetches after startup.
	householdReferenceSegments = 3
)

// Window policy. These are variables rather than constants only so engine
// tests can shorten a run; production code never writes them.
var (
	// HouseholdWarmupMS precedes every scored observation window so
	// connection setup and initial buffering never count as delivery delay.
	HouseholdWarmupMS int64 = 2000
	// HouseholdObserveMS is the target common observation window with every
	// instance active; HouseholdMinObserveMS is the shortest window scored.
	HouseholdObserveMS    int64 = 12000
	HouseholdMinObserveMS int64 = 8000
)

type (
	// HouseholdMix is the per-sensor activity mix. Counts are instances of
	// each activity; browsing is a single background instance.
	HouseholdMix struct {
		UHDStreams int  `json:"uhd_streams"`
		FHDStreams int  `json:"fhd_streams"`
		HDStreams  int  `json:"hd_streams"`
		VideoCalls int  `json:"video_calls"`
		Gaming     int  `json:"gaming"`
		Browsing   bool `json:"browsing"`
	}

	// ActivityProfile is one entry of the versioned activity catalog.
	ActivityProfile struct {
		Key      string
		App      string
		Tier     string
		Label    string
		Pattern  string
		DownMbps float64
		UpMbps   float64
		// IntervalMS is the segment, frame, probe or burst period.
		IntervalMS int64
		// DeadlineMS is how late a unit may arrive before it counts as late.
		DeadlineMS int64
		// ColdBytes, WarmBytes and WarmObjects describe a browsing burst.
		ColdBytes   uint64
		WarmBytes   uint64
		WarmObjects int
		// InFlight bounds concurrent requests for frame-paced activities.
		InFlight int
		// MinSamples is the least evidence a verdict may rest on.
		MinSamples int

		DelayGoodMS, DelayBadMS     float64
		JitterGoodMS, JitterBadMS   float64
		LossGoodPct, LossBadPct     float64
		LateGoodPct, LateBadPct     float64
		StartupGoodMS, StartupBadMS float64
		ColdGoodMS, ColdBadMS       float64
		// Estimate marks activities whose transport cannot reproduce the
		// real protocol; the result is labelled accordingly.
		Estimate bool
		Source   string
	}

	// ActivitySample is one delivery unit observed by the runner.
	ActivitySample struct {
		ScheduledMS int64
		StartedMS   int64
		CompletedMS int64
		FirstByteMS int64
		Bytes       uint64
		Upload      bool
		Cold        bool
		Failed      bool
	}

	// HouseholdActivity is the scored result of one activity instance.
	HouseholdActivity struct {
		Key              string   `json:"key"`
		App              string   `json:"app"`
		Instance         int      `json:"instance"`
		Tier             string   `json:"tier,omitempty"`
		Label            string   `json:"label"`
		Pattern          string   `json:"pattern"`
		DemandDownMbps   float64  `json:"demand_down_mbps"`
		DemandUpMbps     float64  `json:"demand_up_mbps"`
		AchievedDownMbps float64  `json:"achieved_down_mbps"`
		AchievedUpMbps   float64  `json:"achieved_up_mbps"`
		Samples          int      `json:"samples"`
		LateSamples      int      `json:"late_samples"`
		Errors           int      `json:"errors"`
		P50DelayMS       float64  `json:"p50_delay_ms"`
		P90DelayMS       float64  `json:"p90_delay_ms"`
		P95DelayMS       float64  `json:"p95_delay_ms"`
		JitterMS         float64  `json:"jitter_ms"`
		StartupMS        float64  `json:"startup_ms"`
		ColdP95MS        float64  `json:"cold_p95_ms"`
		LatePct          float64  `json:"late_pct"`
		LossPct          float64  `json:"loss_pct"`
		ObservedMS       int64    `json:"observed_ms"`
		Score            float64  `json:"score"`
		Rating           Rating   `json:"rating"`
		Available        bool     `json:"available"`
		Estimate         bool     `json:"estimate"`
		Reasons          []string `json:"reasons,omitempty"`
	}

	HouseholdWindow struct {
		StartMS    int64 `json:"start_ms"`
		EndMS      int64 `json:"end_ms"`
		WarmupMS   int64 `json:"warmup_ms"`
		ObservedMS int64 `json:"observed_ms"`
		PlannedMS  int64 `json:"planned_ms"`
	}

	HouseholdLedger struct {
		CapBytes      uint64 `json:"cap_bytes"`
		PlannedBytes  uint64 `json:"planned_bytes"`
		UsedBytes     uint64 `json:"used_bytes"`
		DownloadBytes uint64 `json:"download_bytes"`
		UploadBytes   uint64 `json:"upload_bytes"`
		ProbeBytes    uint64 `json:"probe_bytes"`
	}

	// HouseholdResult is the wire block attached to household profile results.
	HouseholdResult struct {
		ScenarioVersion int                 `json:"scenario_version"`
		ScorerVersion   int                 `json:"scorer_version"`
		Transport       string              `json:"transport"`
		Mix             HouseholdMix        `json:"mix"`
		MixSource       string              `json:"mix_source"`
		MixRevision     string              `json:"mix_revision"`
		Status          string              `json:"status"`
		Weakest         string              `json:"weakest,omitempty"`
		WeakestScore    float64             `json:"weakest_score"`
		Summary         string              `json:"summary"`
		Window          HouseholdWindow     `json:"window"`
		Ledger          HouseholdLedger     `json:"ledger"`
		StopReason      string              `json:"stop_reason"`
		Reference       *HouseholdActivity  `json:"reference,omitempty"`
		Activities      []HouseholdActivity `json:"activities"`
	}

	// HouseholdPlan is the time and byte plan chosen before a scenario runs.
	HouseholdPlan struct {
		Reference      bool
		ReferenceMS    int64
		WarmupMS       int64
		ObserveMS      int64
		ReferenceBytes uint64
		MixBytes       uint64
		PlannedBytes   uint64
		// Sufficient is false when even the shortest window does not fit the
		// cap or the time available; Reason then names which one
		// (StopInsufficientBytes or StopInsufficientTime) so the result is
		// reported as insufficient regardless of what the run observed.
		Sufficient bool
		Reason     string
	}

	// HouseholdConfidenceInput carries the evidence-quality signals of a
	// household run. Quality and confidence stay independent: a failing mix can
	// be measured with high confidence and a passing one with low confidence.
	HouseholdConfidenceInput struct {
		BaselineSamples       int
		LoadedSamples         int
		ProbeSuccessRatio     float64
		FreshPathSuccessRatio float64
		ObservedMS            int64
		PlannedMS             int64
		StopReason            string
		UnavailableActivities int
	}
)

// DefaultHouseholdMix is two 4K streams, one video call, one game and
// background browsing: the example the product promises to reproduce.
func DefaultHouseholdMix() HouseholdMix {
	return HouseholdMix{UHDStreams: 2, VideoCalls: 1, Gaming: 1, Browsing: true}
}

// NormalizeHouseholdMix clamps counts and falls back to the default when the
// mix is empty or would exceed the instance bound. It never trims a mix
// silently: an over-sized mix is replaced wholesale so the result's recorded
// mix is always exactly what ran.
func NormalizeHouseholdMix(mix HouseholdMix) HouseholdMix {
	mix.UHDStreams = clampInt(mix.UHDStreams, 0, maxHouseholdCount)
	mix.FHDStreams = clampInt(mix.FHDStreams, 0, maxHouseholdCount)
	mix.HDStreams = clampInt(mix.HDStreams, 0, maxHouseholdCount)
	mix.VideoCalls = clampInt(mix.VideoCalls, 0, maxHouseholdCount)
	mix.Gaming = clampInt(mix.Gaming, 0, maxHouseholdCount)
	if instances := mix.Instances(); instances == 0 || instances > MaxHouseholdInstances {
		return DefaultHouseholdMix()
	}
	return mix
}

// Instances counts concurrent activity instances, browsing included.
func (m HouseholdMix) Instances() int {
	total := m.UHDStreams + m.FHDStreams + m.HDStreams + m.VideoCalls + m.Gaming
	if m.Browsing {
		total++
	}
	return total
}

// Revision is a stable identifier for a scenario version plus mix, so history
// can group comparable runs.
func (m HouseholdMix) Revision() string {
	browsing := 0
	if m.Browsing {
		browsing = 1
	}
	return fmt.Sprintf("v%d:uhd%d-fhd%d-hd%d-call%d-game%d-web%d", HouseholdScenarioVersion, m.UHDStreams, m.FHDStreams, m.HDStreams, m.VideoCalls, m.Gaming, browsing)
}

// DemandMbps is the mix's aggregate reference demand per direction.
func (m HouseholdMix) DemandMbps() (down, up float64) {
	for _, activity := range m.Activities() {
		down += activity.DownMbps
		up += activity.UpMbps
	}
	return down, up
}

// Activities expands the mix into one catalog entry per instance, highest
// demand first so instance numbering is stable.
func (m HouseholdMix) Activities() []ActivityProfile {
	out := make([]ActivityProfile, 0, m.Instances())
	add := func(key string, count int) {
		if profile, ok := ActivityByKey(key); ok {
			for range count {
				out = append(out, profile)
			}
		}
	}
	add("stream_uhd", m.UHDStreams)
	add("stream_fhd", m.FHDStreams)
	add("stream_hd", m.HDStreams)
	add("video_call", m.VideoCalls)
	add("game", m.Gaming)
	if m.Browsing {
		add("browsing", 1)
	}
	return out
}

// ReferenceActivity is the single activity measured alone before the mix:
// the highest-demand stream if any, otherwise nothing.
func (m HouseholdMix) ReferenceActivity() (ActivityProfile, bool) {
	switch {
	case m.UHDStreams > 0:
		return ActivityByKey("stream_uhd")
	case m.FHDStreams > 0:
		return ActivityByKey("stream_fhd")
	case m.HDStreams > 0:
		return ActivityByKey("stream_hd")
	}
	return ActivityProfile{}, false
}

// StaggerOffsets spreads the segment fetches of same-kind streams across
// their interval, so two 4K streams do not burst at the same instant the
// way synchronized test instances would but real households never do.
// Continuous activities (frames, probes, bursts) start at once.
func StaggerOffsets(activities []ActivityProfile) []int64 {
	offsets := make([]int64, len(activities))
	counts := map[string]int{}
	for _, activity := range activities {
		if activity.Pattern == PatternSegments {
			counts[activity.Key]++
		}
	}
	seen := map[string]int{}
	for index, activity := range activities {
		if activity.Pattern != PatternSegments || counts[activity.Key] < 2 {
			continue
		}
		offsets[index] = int64(seen[activity.Key]) * activity.IntervalMS / int64(counts[activity.Key])
		seen[activity.Key]++
	}
	return offsets
}

// HouseholdCatalog is activity catalog v1. Reference demands come from
// published service guidance checked on 9 September 2026; the delivery
// thresholds are Pulse policy pending calibration against real players.
func HouseholdCatalog() []ActivityProfile {
	stream := func(key, tier, label string, mbps float64) ActivityProfile {
		return ActivityProfile{
			Key: key, App: AppStreaming, Tier: tier, Label: label, Pattern: PatternSegments,
			DownMbps: mbps, IntervalMS: 2000, DeadlineMS: 2000, InFlight: 1, MinSamples: 3,
			LateGoodPct: 0, LateBadPct: 20, StartupGoodMS: 1500, StartupBadMS: 5000,
			Source: "https://help.netflix.com/en/node/306 (2026-09-09)",
		}
	}
	return []ActivityProfile{
		stream("stream_uhd", "uhd", "4K stream", 15),
		stream("stream_fhd", "fhd", "Full HD stream", 5),
		stream("stream_hd", "hd", "HD stream", 3),
		// Real-time activities are judged on their typical delay (p90) and on
		// the share of units that missed the deadline. A sparse tail of spikes
		// that a jitter buffer absorbs must not decide the verdict; a unit
		// later than the deadline is what a receiver actually drops. In-flight
		// caps cover a full deadline so queueing shows up as lateness, never as
		// synthetic loss.
		{
			Key: "video_call", App: AppVideoCalls, Label: "Video call", Pattern: PatternFrames,
			DownMbps: 3.0, UpMbps: 3.8, IntervalMS: 100, DeadlineMS: 400, InFlight: 5, MinSamples: 50,
			DelayGoodMS: 150, DelayBadMS: 400, JitterGoodMS: 20, JitterBadMS: 80, LossGoodPct: 1, LossBadPct: 8,
			Source: "https://support.zoom.com/hc/en/article?id=zm_kb&sysparm_article=KB0058323 (2026-09-09)",
		},
		{
			Key: "game", App: AppGaming, Label: "Online game", Pattern: PatternProbes,
			DownMbps: 0.1, UpMbps: 0.05, IntervalMS: 50, DeadlineMS: 500, InFlight: 11, MinSamples: 100,
			DelayGoodMS: 60, DelayBadMS: 150, JitterGoodMS: 10, JitterBadMS: 40, LossGoodPct: 1, LossBadPct: 6,
			Estimate: true, Source: "synthetic; HTTPS request timing, calibration pending",
		},
		{
			Key: "browsing", App: AppBrowsing, Label: "Browsing", Pattern: PatternBursts,
			DownMbps: 0.75, IntervalMS: 3000, DeadlineMS: 1000, ColdBytes: 20 << 10, WarmBytes: 50 << 10, WarmObjects: 5, InFlight: 2, MinSamples: 6,
			DelayGoodMS: 150, DelayBadMS: 600, ColdGoodMS: 400, ColdBadMS: 1200, LossGoodPct: 0, LossBadPct: 5,
			Source: "synthetic; small-object page bursts",
		},
	}
}

// ActivityByKey looks up a catalog entry.
func ActivityByKey(key string) (ActivityProfile, bool) {
	for _, profile := range HouseholdCatalog() {
		if profile.Key == key {
			return profile, true
		}
	}
	return ActivityProfile{}, false
}

// UnitBytes is the payload of one delivery unit in the given direction.
func (p ActivityProfile) UnitBytes(upload bool) uint64 {
	rate := p.DownMbps
	if upload {
		rate = p.UpMbps
	}
	switch p.Pattern {
	case PatternSegments, PatternFrames:
		return uint64(rate * float64(p.IntervalMS) / 8 * 1000)
	case PatternProbes:
		return 0
	case PatternBursts:
		return p.ColdBytes + p.WarmBytes*uint64(p.WarmObjects)
	}
	return 0
}

// PlannedBytes estimates payload for running the activity for the duration.
func (p ActivityProfile) PlannedBytes(duration int64) uint64 {
	if duration <= 0 {
		return 0
	}
	seconds := float64(duration) / 1000
	bytes := (p.DownMbps + p.UpMbps) * seconds / 8 * 1_000_000
	if p.Pattern == PatternProbes {
		// Tiny requests still cost headers each way.
		bytes = float64(duration/p.IntervalMS) * 600
	}
	return uint64(math.Ceil(bytes))
}

// PlanHousehold chooses the reference and observation window that fit the
// byte cap and the time available. The requested mix is never reduced: when
// even the shortest window does not fit, the plan is marked insufficient and
// the scenario still runs so the shortfall is observed, not invented.
func PlanHousehold(mix HouseholdMix, capBytes uint64, availableMS int64) HouseholdPlan {
	plan := HouseholdPlan{WarmupMS: HouseholdWarmupMS, ObserveMS: HouseholdObserveMS, Sufficient: true}
	usable := uint64(float64(capBytes) * (1 - HouseholdOverheadReserve))
	activities := mix.Activities()
	mixBytes := func(observe int64) uint64 {
		var total uint64
		for _, activity := range activities {
			total += activity.PlannedBytes(plan.WarmupMS + observe)
		}
		return total
	}
	if reference, ok := mix.ReferenceActivity(); ok {
		plan.ReferenceMS = reference.IntervalMS*householdReferenceSegments + reference.IntervalMS/2
		plan.ReferenceBytes = reference.UnitBytes(false) * (householdReferenceSegments + 1)
		plan.Reference = true
	}
	fits := func() bool {
		plan.MixBytes = mixBytes(plan.ObserveMS)
		plan.PlannedBytes = plan.MixBytes
		total := plan.WarmupMS + plan.ObserveMS
		if plan.Reference {
			plan.PlannedBytes += plan.ReferenceBytes
			total += plan.ReferenceMS
		}
		return plan.PlannedBytes <= usable && total <= availableMS
	}
	if fits() {
		return plan
	}
	plan.Reference, plan.ReferenceBytes, plan.ReferenceMS = false, 0, 0
	if fits() {
		return plan
	}
	// Shrink the window one second at a time down to the minimum.
	for plan.ObserveMS > HouseholdMinObserveMS {
		plan.ObserveMS -= 1000
		if fits() {
			return plan
		}
	}
	fits()
	plan.Sufficient = false
	plan.Reason = StopInsufficientTime
	if plan.PlannedBytes > usable {
		plan.Reason = StopInsufficientBytes
	}
	return plan
}

// SummarizeActivity turns the samples of one instance observed between
// windowStart and windowEnd (test-relative milliseconds) into a scored
// result. Samples scheduled outside the window are ignored.
func SummarizeActivity(profile ActivityProfile, instance int, samples []ActivitySample, windowStart, windowEnd int64) HouseholdActivity {
	activity := HouseholdActivity{
		Key: profile.Key, App: profile.App, Instance: instance, Tier: profile.Tier, Label: profile.Label, Pattern: profile.Pattern,
		DemandDownMbps: profile.DownMbps, DemandUpMbps: profile.UpMbps, Estimate: profile.Estimate,
		ObservedMS: max(windowEnd-windowStart, 0),
	}
	delays := make([]float64, 0, len(samples))
	colds := make([]float64, 0, 8)
	var downBytes, upBytes uint64
	// occupiedMS is how long the segments of a stream actually took to
	// deliver, each taking at least its interval: an on-time stream delivers
	// exactly its demand, a late one less, and a partial window never
	// under-reports a stream that was on schedule.
	var occupiedMS int64
	startup := -1.0
	for _, sample := range samples {
		if sample.ScheduledMS < windowStart || sample.ScheduledMS >= windowEnd {
			if profile.Pattern == PatternSegments && startup < 0 && !sample.Failed && sample.ScheduledMS < windowStart {
				startup = float64(sample.CompletedMS - sample.ScheduledMS)
			}
			continue
		}
		activity.Samples++
		if sample.Failed {
			activity.Errors++
			continue
		}
		if sample.Upload {
			upBytes += sample.Bytes
		} else {
			downBytes += sample.Bytes
		}
		var delay float64
		switch profile.Pattern {
		case PatternBursts:
			if sample.Cold {
				colds = append(colds, float64(sample.CompletedMS-sample.ScheduledMS))
				continue
			}
			delay = float64(sample.FirstByteMS - sample.ScheduledMS)
		default:
			delay = float64(sample.CompletedMS - sample.ScheduledMS)
		}
		delay = max(delay, 0)
		if profile.Pattern == PatternSegments {
			occupiedMS += max(profile.IntervalMS, int64(delay))
		}
		if delay > float64(profile.DeadlineMS) {
			activity.LateSamples++
		}
		delays = append(delays, delay)
	}
	if activity.Samples == 0 {
		activity.Reasons = []string{"no_samples"}
		activity.Rating = RatingUnavailable
		return activity
	}
	seconds := float64(activity.ObservedMS) / 1000
	if profile.Pattern == PatternSegments && occupiedMS > 0 {
		seconds = float64(occupiedMS) / 1000
	}
	if seconds > 0 {
		activity.AchievedDownMbps = round(float64(downBytes)*8/seconds/1_000_000, 2)
		activity.AchievedUpMbps = round(float64(upBytes)*8/seconds/1_000_000, 2)
	}
	if len(delays) > 0 {
		sort.Float64s(delays)
		activity.P50DelayMS = round(percentile(delays, 50), 1)
		activity.P90DelayMS = round(percentile(delays, 90), 1)
		activity.P95DelayMS = round(percentile(delays, 95), 1)
		// Robust jitter: the median absolute deviation describes the timing
		// a jitter buffer actually tracks; a few spikes belong to the late
		// share, not to jitter.
		activity.JitterMS = round(medianAbsoluteDeviation(delays), 1)
	}
	if len(colds) > 0 {
		sort.Float64s(colds)
		activity.ColdP95MS = round(percentile(colds, 95), 1)
	}
	if startup >= 0 {
		activity.StartupMS = round(startup, 1)
	}
	activity.LatePct = round(100*float64(activity.LateSamples)/float64(activity.Samples), 2)
	activity.LossPct = round(100*float64(activity.Errors)/float64(activity.Samples), 2)
	ScoreHouseholdActivity(profile, &activity)
	return activity
}

// ScoreHouseholdActivity applies the scorer policy for the activity's pattern.
// Throughput earns no credit beyond demand: a gigabit line and a 20 Mbps line
// score the same 4K stream identically when both deliver every segment.
func ScoreHouseholdActivity(profile ActivityProfile, activity *HouseholdActivity) {
	if activity.Samples < profile.MinSamples {
		activity.Available = false
		activity.Rating = RatingUnavailable
		activity.Reasons = appendUnique(activity.Reasons, "insufficient_samples")
		return
	}
	activity.Available = true
	var score float64
	reasons := activity.Reasons[:0]
	switch profile.Pattern {
	case PatternSegments:
		late := lowerIsBetter(activity.LatePct, profile.LateGoodPct, profile.LateBadPct)
		rate := higherIsBetter(activity.AchievedDownMbps, profile.DownMbps*0.7, profile.DownMbps*0.98)
		startup := 100.0
		if activity.StartupMS > 0 {
			startup = lowerIsBetter(activity.StartupMS, profile.StartupGoodMS, profile.StartupBadMS)
		}
		loss := lowerIsBetter(activity.LossPct, 0, 10)
		score = 0.5*late + 0.3*rate + 0.1*startup + 0.1*loss
		if late < 80 {
			reasons = append(reasons, "segments_late")
		}
		if rate < 80 {
			reasons = append(reasons, "throughput_below_demand")
		}
		if startup < 80 {
			reasons = append(reasons, "slow_startup")
		}
		if loss < 80 {
			reasons = append(reasons, "segment_errors")
		}
	case PatternFrames, PatternProbes:
		// Typical delay (p90) and the missed-deadline share carry the verdict;
		// the p95 stays on the record for diagnosis.
		delay := lowerIsBetter(activity.P90DelayMS, profile.DelayGoodMS, profile.DelayBadMS)
		jitter := lowerIsBetter(activity.JitterMS, profile.JitterGoodMS, profile.JitterBadMS)
		loss := lowerIsBetter(activity.LossPct+activity.LatePct, profile.LossGoodPct, profile.LossBadPct)
		if profile.Pattern == PatternFrames {
			score = 0.4*delay + 0.2*jitter + 0.4*loss
		} else {
			score = 0.5*delay + 0.2*jitter + 0.3*loss
		}
		if delay < 80 {
			reasons = append(reasons, "delivery_delay")
		}
		if jitter < 80 {
			reasons = append(reasons, "jitter")
		}
		if loss < 80 {
			reasons = append(reasons, "lost_or_late_frames")
		}
	case PatternBursts:
		warm := lowerIsBetter(activity.P90DelayMS, profile.DelayGoodMS, profile.DelayBadMS)
		cold := 100.0
		if activity.ColdP95MS > 0 {
			cold = lowerIsBetter(activity.ColdP95MS, profile.ColdGoodMS, profile.ColdBadMS)
		}
		loss := lowerIsBetter(activity.LossPct, profile.LossGoodPct, profile.LossBadPct)
		score = 0.5*warm + 0.3*cold + 0.2*loss
		if warm < 80 {
			reasons = append(reasons, "slow_response_start")
		}
		if cold < 80 {
			reasons = append(reasons, "slow_connection_setup")
		}
		if loss < 80 {
			reasons = append(reasons, "request_errors")
		}
	}
	activity.Score = round(clamp(score, 0, 100), 1)
	activity.Rating = rating(activity.Score)
	activity.Reasons = reasons
}

// SummarizeHousehold derives status, weakest activity and summary text from
// the scored activities and the stop reason. It never averages: the weakest
// requested activity decides.
func SummarizeHousehold(result *HouseholdResult) {
	result.ScenarioVersion = HouseholdScenarioVersion
	result.ScorerVersion = HouseholdScorerVersion
	result.MixRevision = result.Mix.Revision()
	weakest := (*HouseholdActivity)(nil)
	insufficient := false
	for index := range result.Activities {
		activity := &result.Activities[index]
		if !activity.Available {
			insufficient = true
			continue
		}
		if weakest == nil || activity.Score < weakest.Score {
			weakest = activity
		}
	}
	if weakest == nil || result.StopReason != StopEnoughEvidence || result.Window.ObservedMS < HouseholdMinObserveMS {
		result.Status = HouseholdStatusInsufficient
	} else {
		switch weakest.Rating {
		case RatingGood:
			result.Status = HouseholdStatusPass
		case RatingAverage:
			result.Status = HouseholdStatusStruggle
		default:
			result.Status = HouseholdStatusFail
		}
		if insufficient {
			result.Status = HouseholdStatusInsufficient
		}
	}
	if weakest != nil {
		result.Weakest = weakest.Key
		result.WeakestScore = weakest.Score
	}
	result.Summary = householdSummary(result, weakest)
}

func householdSummary(result *HouseholdResult, weakest *HouseholdActivity) string {
	if result.Status == HouseholdStatusInsufficient {
		switch result.StopReason {
		case StopInsufficientBytes:
			return "The test ran out of its data allowance before enough evidence was collected."
		case StopInsufficientTime:
			return "The test ran out of time before enough evidence was collected."
		case StopTransportFailure:
			return "The measurement endpoint could not be reached reliably."
		case StopCancelled:
			return "The test was cancelled before it finished."
		}
		return "Not enough evidence to judge this household mix."
	}
	reference := ""
	if result.Reference != nil && result.Reference.Available {
		reference = fmt.Sprintf("One %s alone %s. ", result.Reference.Label, verdictVerb(result.Reference.Rating))
	}
	if weakest == nil {
		return reference + "No activity could be scored."
	}
	switch result.Status {
	case HouseholdStatusPass:
		return reference + "The configured mix ran together without a weak activity."
	default:
		reason := "no dominant cause"
		if len(weakest.Reasons) > 0 {
			reason = reasonText(weakest.Reasons[0])
		}
		return fmt.Sprintf("%sThe configured mix %s because the %s saw %s.", reference, verdictVerb(weakest.Rating), weakest.Label, reason)
	}
}

func verdictVerb(rating Rating) string {
	switch rating {
	case RatingGood:
		return "passed"
	case RatingAverage:
		return "struggled"
	default:
		return "failed"
	}
}

func reasonText(reason string) string {
	switch reason {
	case "segments_late":
		return "late video segments"
	case "throughput_below_demand":
		return "throughput below its demand"
	case "slow_startup":
		return "a slow start"
	case "delivery_delay":
		return "delayed delivery"
	case "jitter":
		return "unsteady delivery timing"
	case "lost_or_late_frames":
		return "lost or late frames"
	case "slow_response_start":
		return "slow responses"
	case "slow_connection_setup":
		return "slow connection setup"
	default:
		return reason
	}
}

// HouseholdExperience fills the everyday cards from household activities so
// existing consumers keep working. Streaming takes the weakest stream. The
// overall score is the weakest requested activity, never an average.
func HouseholdExperience(result HouseholdResult) Experience {
	experience := Experience{Home: map[string]ApplicationScore{}}
	labels := map[string]string{AppBrowsing: "Browsing", AppStreaming: "Streaming", AppVideoCalls: "Video calls", AppGaming: "Gaming"}
	explanations := map[string]string{
		AppBrowsing:   "Pages and requests during the household mix.",
		AppStreaming:  "Weakest configured stream while the mix ran.",
		AppVideoCalls: "Frame delivery both ways while the mix ran.",
		AppGaming:     "Round-trip timing under the household mix (HTTPS estimate).",
	}
	for _, app := range []string{AppBrowsing, AppStreaming, AppVideoCalls, AppGaming} {
		card := ApplicationScore{Name: labels[app], Rating: RatingUnavailable, Explanation: explanations[app]}
		var chosen *HouseholdActivity
		for index := range result.Activities {
			activity := &result.Activities[index]
			if activity.App != app {
				continue
			}
			if chosen == nil || (activity.Available && (!chosen.Available || activity.Score < chosen.Score)) {
				chosen = activity
			}
		}
		switch {
		case chosen == nil:
			card.UnavailableReasons = []string{"not_in_mix"}
		case !chosen.Available:
			card.UnavailableReasons = append([]string(nil), chosen.Reasons...)
		default:
			card.Available = true
			card.Score = chosen.Score
			card.Rating = chosen.Rating
			card.LatencyScore = round(lowerIsBetterFor(chosen), 1)
			card.LossScore = round(lowerIsBetter(chosen.LossPct+chosen.LatePct, 0, 5), 1)
			card.ThroughputScore = round(throughputScoreFor(chosen), 1)
			card.MeasuredLatencyMS = chosen.P95DelayMS
			card.MeasuredLossPct = chosen.LossPct
			card.DownloadMbps = chosen.AchievedDownMbps
			card.UploadMbps = chosen.AchievedUpMbps
			if len(chosen.Reasons) > 0 {
				card.Explanation = "Weakest issue: " + reasonText(chosen.Reasons[0]) + "."
			}
		}
		experience.Home[app] = card
	}
	if result.Weakest != "" && result.Status != HouseholdStatusInsufficient {
		experience.Available = true
		experience.OverallScore = result.WeakestScore
		experience.Rating = rating(result.WeakestScore)
	} else {
		experience.Rating = RatingUnavailable
	}
	return experience
}

func lowerIsBetterFor(activity *HouseholdActivity) float64 {
	profile, ok := ActivityByKey(activity.Key)
	if !ok || profile.DelayBadMS <= profile.DelayGoodMS {
		return lowerIsBetter(activity.LatePct, 0, 20)
	}
	return lowerIsBetter(activity.P95DelayMS, profile.DelayGoodMS, profile.DelayBadMS)
}

func throughputScoreFor(activity *HouseholdActivity) float64 {
	demand := activity.DemandDownMbps + activity.DemandUpMbps
	if demand <= 0 {
		return 100
	}
	achieved := activity.AchievedDownMbps + activity.AchievedUpMbps
	return higherIsBetter(achieved, demand*0.7, demand*0.98)
}

// HouseholdConfidence grades how much the household verdict can be trusted.
func HouseholdConfidence(input HouseholdConfidenceInput) Confidence {
	reasons := []string{}
	severity := 0
	add := func(reason string, level int) {
		if !slices.Contains(reasons, reason) {
			reasons = append(reasons, reason)
		}
		severity = max(severity, level)
	}
	if input.BaselineSamples < MinimumBaselineSamples {
		add("baseline_insufficient_samples", 1)
	}
	if input.LoadedSamples < 20 {
		add("household_insufficient_latency_samples", 1)
	}
	if input.ProbeSuccessRatio < .7 {
		add("too_few_latency_probes", 2)
	} else if input.ProbeSuccessRatio < .9 {
		add("some_latency_probes_missing", 1)
	}
	if input.FreshPathSuccessRatio > 0 && input.FreshPathSuccessRatio < .7 {
		add("fresh_path_probes_unreliable", 1)
	}
	if input.ObservedMS < HouseholdMinObserveMS {
		add("household_window_too_short", 2)
	} else if input.PlannedMS > 0 && input.ObservedMS < input.PlannedMS {
		add("household_window_shortened", 1)
	}
	switch input.StopReason {
	case StopInsufficientBytes:
		add("household_budget_limited", 1)
	case StopInsufficientTime:
		add("household_time_limited", 1)
	case StopTransportFailure, StopCancelled:
		add("household_"+input.StopReason, 2)
	}
	if input.UnavailableActivities > 0 {
		add("household_activities_unscored", 1)
	}
	level := "high"
	if severity == 1 {
		level = "medium"
	} else if severity >= 2 {
		level = "low"
	}
	return Confidence{Level: level, Reasons: reasons}
}

func percentile(sorted []float64, pct float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := pct / 100 * float64(len(sorted)-1)
	low := int(math.Floor(rank))
	high := min(low+1, len(sorted)-1)
	return sorted[low] + (sorted[high]-sorted[low])*(rank-float64(low))
}

func appendUnique(values []string, value string) []string {
	if slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}

func clampInt(value, low, high int) int { return min(max(value, low), high) }
