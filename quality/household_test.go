package quality

import (
	"slices"
	"strings"
	"testing"
)

func TestNormalizeHouseholdMixClampsAndFallsBack(t *testing.T) {
	t.Parallel()
	if got := NormalizeHouseholdMix(HouseholdMix{}); got != DefaultHouseholdMix() {
		t.Fatalf("empty mix = %+v, want default", got)
	}
	if got := NormalizeHouseholdMix(HouseholdMix{UHDStreams: 9, VideoCalls: -3}); got != (HouseholdMix{UHDStreams: 4}) {
		t.Fatalf("clamped mix = %+v", got)
	}
	// Over the instance bound the whole mix is replaced, never trimmed, so
	// the recorded mix is always exactly what ran.
	if got := NormalizeHouseholdMix(HouseholdMix{UHDStreams: 4, FHDStreams: 4, Gaming: 1}); got != DefaultHouseholdMix() {
		t.Fatalf("oversized mix = %+v, want default", got)
	}
	mix := DefaultHouseholdMix()
	if mix.Instances() != 5 || mix.Revision() != "v1:uhd2-fhd0-hd0-call1-game1-web1" {
		t.Fatalf("instances=%d revision=%s", mix.Instances(), mix.Revision())
	}
	down, up := mix.DemandMbps()
	if down < 33 || down > 34.5 || up < 3.8 || up > 4 {
		t.Fatalf("demand down=%.2f up=%.2f", down, up)
	}
}

func TestPlanHouseholdDropsReferenceThenShrinksWindow(t *testing.T) {
	t.Parallel()
	mix := DefaultHouseholdMix()
	generous := PlanHousehold(mix, 300_000_000, 30_000)
	if !generous.Reference || generous.ObserveMS != HouseholdObserveMS || !generous.Sufficient {
		t.Fatalf("generous plan = %+v", generous)
	}
	if generous.PlannedBytes > 100_000_000 {
		t.Fatalf("default mix planned %d bytes; expected well under the cap", generous.PlannedBytes)
	}
	// Not enough time for the reference: keep the full window, drop it.
	short := PlanHousehold(mix, 300_000_000, 15_000)
	if short.Reference || short.ObserveMS != HouseholdObserveMS || !short.Sufficient {
		t.Fatalf("time-limited plan = %+v", short)
	}
	// A cap that cannot fund the full window shrinks it but never below the
	// minimum, and never reduces the requested mix.
	tight := PlanHousehold(mix, 60_000_000, 30_000)
	if tight.Reference || tight.ObserveMS >= HouseholdObserveMS || tight.ObserveMS < HouseholdMinObserveMS || !tight.Sufficient {
		t.Fatalf("byte-limited plan = %+v", tight)
	}
	starved := PlanHousehold(mix, 5_000_000, 30_000)
	if starved.Sufficient || starved.ObserveMS != HouseholdMinObserveMS || starved.Reason != StopInsufficientBytes {
		t.Fatalf("starved plan = %+v", starved)
	}
	rushed := PlanHousehold(mix, 300_000_000, 6_000)
	if rushed.Sufficient || rushed.Reason != StopInsufficientTime {
		t.Fatalf("rushed plan = %+v", rushed)
	}
}

func segmentSamples(count int, fetchMS int64) []ActivitySample {
	samples := make([]ActivitySample, 0, count+1)
	// Startup segment before the window.
	samples = append(samples, ActivitySample{ScheduledMS: 0, StartedMS: 0, CompletedMS: 900, Bytes: 3_750_000})
	scheduled := int64(2000)
	for range count {
		completed := scheduled + fetchMS
		samples = append(samples, ActivitySample{ScheduledMS: scheduled, StartedMS: scheduled, CompletedMS: completed, Bytes: 3_750_000})
		scheduled += 2000
		if completed > scheduled {
			scheduled = completed
		}
	}
	return samples
}

func TestStreamScoringRewardsDeliveryNotExcessThroughput(t *testing.T) {
	t.Parallel()
	profile, _ := ActivityByKey("stream_uhd")
	good := SummarizeActivity(profile, 1, segmentSamples(6, 400), 2000, 14000)
	if !good.Available || good.Rating != RatingGood || good.LateSamples != 0 || good.StartupMS != 900 {
		t.Fatalf("healthy stream = %+v", good)
	}
	// Every segment arriving after the next is due: late and below demand.
	late := SummarizeActivity(profile, 1, segmentSamples(6, 3000), 2000, 14000)
	if late.Rating != RatingBad || late.LatePct < 90 || late.AchievedDownMbps >= profile.DownMbps*0.98 {
		t.Fatalf("late stream = %+v", late)
	}
	if !contains(late.Reasons, "segments_late") {
		t.Fatalf("late stream reasons = %v", late.Reasons)
	}
	// Two segments in the window is not enough evidence for a verdict.
	thin := SummarizeActivity(profile, 1, segmentSamples(2, 400), 2000, 14000)
	if thin.Available || thin.Rating != RatingUnavailable || !contains(thin.Reasons, "insufficient_samples") {
		t.Fatalf("thin stream = %+v", thin)
	}
}

func frameSamples(count int, delayMS int64, failEvery int) []ActivitySample {
	samples := make([]ActivitySample, 0, count)
	for index := range count {
		scheduled := int64(2000 + index*100)
		sample := ActivitySample{ScheduledMS: scheduled, StartedMS: scheduled, CompletedMS: scheduled + delayMS, Bytes: 37_500, Upload: index%2 == 1}
		if failEvery > 0 && index%failEvery == 0 {
			sample.Failed = true
		}
		samples = append(samples, sample)
	}
	return samples
}

func TestCallAndGameScoringUseDelayJitterAndLoss(t *testing.T) {
	t.Parallel()
	call, _ := ActivityByKey("video_call")
	clean := SummarizeActivity(call, 1, frameSamples(120, 60, 0), 2000, 14000)
	if clean.Rating != RatingGood || clean.LossPct != 0 || clean.P95DelayMS != 60 {
		t.Fatalf("clean call = %+v", clean)
	}
	lossy := SummarizeActivity(call, 1, frameSamples(120, 60, 10), 2000, 14000)
	if lossy.LossPct < 9 || lossy.Rating == RatingGood || !contains(lossy.Reasons, "lost_or_late_frames") {
		t.Fatalf("lossy call = %+v", lossy)
	}
	slow := SummarizeActivity(call, 1, frameSamples(120, 450, 0), 2000, 14000)
	if slow.Rating != RatingBad || slow.LatePct < 99 {
		t.Fatalf("slow call = %+v", slow)
	}
	game, _ := ActivityByKey("game")
	probes := make([]ActivitySample, 0, 200)
	for index := range 200 {
		scheduled := int64(2000 + index*50)
		probes = append(probes, ActivitySample{ScheduledMS: scheduled, StartedMS: scheduled, CompletedMS: scheduled + 30})
	}
	responsive := SummarizeActivity(game, 1, probes, 2000, 14000)
	if responsive.Rating != RatingGood || !responsive.Estimate {
		t.Fatalf("responsive game = %+v", responsive)
	}
}

func TestBrowsingScoringSeparatesColdSetupFromWarmResponses(t *testing.T) {
	t.Parallel()
	browsing, _ := ActivityByKey("browsing")
	samples := []ActivitySample{}
	for burst := range 4 {
		base := int64(2000 + burst*3000)
		samples = append(samples, ActivitySample{ScheduledMS: base, StartedMS: base, CompletedMS: base + 350, FirstByteMS: base + 300, Bytes: 20 << 10, Cold: true})
		for object := range 5 {
			at := base + 400 + int64(object)*60
			samples = append(samples, ActivitySample{ScheduledMS: at, StartedMS: at, CompletedMS: at + 50, FirstByteMS: at + 40, Bytes: 50 << 10})
		}
	}
	got := SummarizeActivity(browsing, 1, samples, 2000, 14000)
	if got.Rating != RatingGood || got.ColdP95MS != 350 || got.P95DelayMS != 40 {
		t.Fatalf("browsing = %+v", got)
	}
}

func TestHouseholdStatusIsDecidedByTheWeakestActivity(t *testing.T) {
	t.Parallel()
	result := HouseholdResult{
		Mix: DefaultHouseholdMix(), StopReason: StopEnoughEvidence,
		Window: HouseholdWindow{ObservedMS: HouseholdObserveMS, PlannedMS: HouseholdObserveMS},
		Activities: []HouseholdActivity{
			{Key: "stream_uhd", App: AppStreaming, Instance: 1, Label: "4K stream", Score: 96, Rating: RatingGood, Available: true},
			{Key: "stream_uhd", App: AppStreaming, Instance: 2, Label: "4K stream", Score: 91, Rating: RatingGood, Available: true},
			{Key: "video_call", App: AppVideoCalls, Instance: 1, Label: "Video call", Score: 41, Rating: RatingBad, Available: true, Reasons: []string{"delivery_delay"}, P95DelayMS: 520},
			{Key: "game", App: AppGaming, Instance: 1, Label: "Online game", Score: 88, Rating: RatingGood, Available: true, Estimate: true},
		},
	}
	SummarizeHousehold(&result)
	if result.Status != HouseholdStatusFail || result.Weakest != "video_call" || result.WeakestScore != 41 {
		t.Fatalf("summary = %+v", result)
	}
	if !strings.Contains(result.Summary, "Video call") || !strings.Contains(result.Summary, "delayed delivery") {
		t.Fatalf("summary text = %q", result.Summary)
	}
	experience := HouseholdExperience(result)
	if !experience.Available || experience.OverallScore != 41 || experience.Rating != RatingBad {
		t.Fatalf("experience = %+v", experience)
	}
	if experience.Home[AppStreaming].Score != 91 || experience.Home[AppVideoCalls].Rating != RatingBad {
		t.Fatalf("home cards = %+v", experience.Home)
	}
	if browsing := experience.Home[AppBrowsing]; browsing.Available || browsing.UnavailableReasons[0] != "not_in_mix" {
		t.Fatalf("browsing card = %+v", browsing)
	}

	// A budget-limited run is insufficient regardless of the scores.
	limited := result
	limited.StopReason = StopInsufficientBytes
	SummarizeHousehold(&limited)
	if limited.Status != HouseholdStatusInsufficient || HouseholdExperience(limited).Available {
		t.Fatalf("limited = %+v", limited)
	}
	// One unscored activity also blocks a verdict: silence is not a pass.
	unscored := result
	unscored.Activities = append([]HouseholdActivity(nil), result.Activities...)
	unscored.Activities[3].Available = false
	SummarizeHousehold(&unscored)
	if unscored.Status != HouseholdStatusInsufficient {
		t.Fatalf("unscored = %+v", unscored)
	}
}

func TestHouseholdConfidenceIsIndependentOfQuality(t *testing.T) {
	t.Parallel()
	high := HouseholdConfidence(HouseholdConfidenceInput{BaselineSamples: 20, LoadedSamples: 100, ProbeSuccessRatio: 1, ObservedMS: 12000, PlannedMS: 12000, StopReason: StopEnoughEvidence})
	if high.Level != "high" || len(high.Reasons) != 0 {
		t.Fatalf("high = %+v", high)
	}
	low := HouseholdConfidence(HouseholdConfidenceInput{BaselineSamples: 20, LoadedSamples: 100, ProbeSuccessRatio: 1, ObservedMS: 4000, PlannedMS: 12000, StopReason: StopInsufficientTime})
	if low.Level != "low" || !contains(low.Reasons, "household_window_too_short") || !contains(low.Reasons, "household_time_limited") {
		t.Fatalf("low = %+v", low)
	}
}

func TestBidirectionalStabilityRequiresBothDirections(t *testing.T) {
	t.Parallel()
	steady := func(mbps float64, count int) []Bucket {
		buckets := make([]Bucket, 0, count)
		for index := range count {
			buckets = append(buckets, Bucket{Index: index, DurationMS: 1000, Bytes: uint64(mbps * 125_000), Samples: 4, StreamCount: 2, Complete: true})
		}
		return buckets
	}
	both := DetectBidirectionalStableRegion(steady(80, 5), steady(20, 5), DefaultStabilityPolicy())
	if !both.Stable || both.ThroughputMbps != 100 || both.BucketCount != 5 {
		t.Fatalf("both stable = %+v", both)
	}
	// Download climbs while upload collapses by the same amount: the sum is
	// flat, but neither direction is stable.
	download := steady(50, 5)
	upload := steady(50, 5)
	for index := range 5 {
		download[index].Bytes = uint64((50 + float64(index)*10) * 125_000)
		upload[index].Bytes = uint64((50 - float64(index)*10) * 125_000)
	}
	if sum := DetectStableRegion(combine(download, upload), DefaultStabilityPolicy()); !sum.Stable {
		t.Fatalf("test premise broken: summed buckets should look stable, got %+v", sum)
	}
	crossing := DetectBidirectionalStableRegion(download, upload, DefaultStabilityPolicy())
	if crossing.Stable || crossing.Reason != "neither_direction_stable" {
		t.Fatalf("crossing = %+v", crossing)
	}
	// Upload ran out of bytes after two buckets: the directions never
	// overlap for the required count.
	truncated := DetectBidirectionalStableRegion(steady(80, 5), steady(20, 2), DefaultStabilityPolicy())
	if truncated.Stable || truncated.Reason != "insufficient_complete_buckets" {
		t.Fatalf("truncated = %+v", truncated)
	}
	late := append([]Bucket{}, Bucket{Index: 3, DurationMS: 1000, Bytes: 2_500_000, Samples: 4, StreamCount: 2, Complete: true}, Bucket{Index: 4, DurationMS: 1000, Bytes: 2_500_000, Samples: 4, StreamCount: 2, Complete: true}, Bucket{Index: 5, DurationMS: 1000, Bytes: 2_500_000, Samples: 4, StreamCount: 2, Complete: true})
	shifted := DetectBidirectionalStableRegion(steady(80, 4), late, DefaultStabilityPolicy())
	if shifted.Stable || shifted.Reason != "directions_not_overlapping" {
		t.Fatalf("shifted = %+v", shifted)
	}
}

func combine(left, right []Bucket) []Bucket {
	out := make([]Bucket, len(left))
	for index := range left {
		out[index] = left[index]
		out[index].Bytes += right[index].Bytes
	}
	return out
}

func contains(values []string, value string) bool { return slices.Contains(values, value) }

func TestStaggerOffsetsSpreadOnlySameKindStreams(t *testing.T) {
	t.Parallel()
	offsets := StaggerOffsets((HouseholdMix{UHDStreams: 2, HDStreams: 1, VideoCalls: 1, Gaming: 1, Browsing: true}).Activities())
	// Two 4K streams interleave across their 2 s interval; the lone HD stream
	// and every continuous activity start at once.
	if want := []int64{0, 1000, 0, 0, 0, 0}; !slices.Equal(offsets, want) {
		t.Fatalf("offsets = %v, want %v", offsets, want)
	}
	if got := StaggerOffsets((HouseholdMix{FHDStreams: 4}).Activities()); !slices.Equal(got, []int64{0, 500, 1000, 1500}) {
		t.Fatalf("four streams = %v", got)
	}
}

func TestRealTimeScoringToleratesASparseTailOfSpikes(t *testing.T) {
	t.Parallel()
	call, _ := ActivityByKey("video_call")
	// 5 % of frames spike to 350 ms on an otherwise 45 ms path: what a Wi-Fi
	// link does when idle. A jitter buffer absorbs this; the verdict must not
	// call the call unusable.
	samples := frameSamples(240, 45, 0)
	for index := range samples {
		if index%16 == 0 {
			samples[index].CompletedMS = samples[index].ScheduledMS + 350
		}
	}
	spiky := SummarizeActivity(call, 1, samples, 2000, 26000)
	if spiky.P95DelayMS < 300 || spiky.P90DelayMS > 60 || spiky.JitterMS > 5 {
		t.Fatalf("distribution = %+v", spiky)
	}
	if spiky.Rating != RatingGood {
		t.Fatalf("sparse spikes must not fail a call: %+v", spiky)
	}
	// One frame in ten past the 400 ms deadline is real loss and must not pass.
	late := frameSamples(240, 45, 0)
	for index := range late {
		if index%10 == 0 {
			late[index].CompletedMS = late[index].ScheduledMS + 450
		}
	}
	lossy := SummarizeActivity(call, 1, late, 2000, 26000)
	if lossy.LatePct < 9 || lossy.Rating == RatingGood {
		t.Fatalf("ten percent late frames rated %s: %+v", lossy.Rating, lossy)
	}
}
