// Package measurement provides speed-test gating and local link context.
package measurement

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Taurine-Technology/axon-contracts/gen/go/diagnostics"
	"github.com/Taurine-Technology/axon-pulse/internal/protocol"
	"github.com/Taurine-Technology/axon-pulse/internal/qualitytest"
	"github.com/Taurine-Technology/axon-pulse/quality"
)

type (
	LocalContext struct {
		InterfaceType    string
		LinkMbps         float64
		TXLinkMbps       float64
		RXLinkMbps       float64
		RSSIDBm          float64
		NoiseDBm         float64
		SNRDB            float64
		FrequencyMHz     int
		Channel          int
		ChannelWidthMHz  int
		PHY              string
		Security         string
		SSID             string
		Metered          bool
		OnBattery        bool
		BatteryPct       float64
		ContextAvailable bool
		Gateway          string
		NetworkIdentity  string
		PowerAvailable   bool
		MeteredAvailable bool
	}

	Collector struct {
		mu              sync.Mutex
		lastGateway     string
		lastAssociation string
	}

	SpeedRunner struct {
		Run func(context.Context, diagnostics.SpeedTestOptions) diagnostics.SpeedTestResult
		// Progress receives transient live snapshots while a test runs; nil
		// disables streaming. Injected Run implementations do not emit progress.
		Progress func(protocol.SpeedTestProgress)
	}

	// HouseholdRequest names the activity mix a household test reproduces and
	// where that mix came from; it is ignored by every other profile.
	HouseholdRequest struct {
		Mix    quality.HouseholdMix
		Source string
	}
)

func parseMeteredValue(value string) (metered, available bool) {
	// nmcli -g prints guessed states as "yes (guessed)" / "no (guessed)".
	normalized := strings.ToLower(strings.TrimSpace(value))
	normalized = strings.TrimSuffix(normalized, " (guessed)")
	switch normalized {
	case "yes", "true", "fixed", "variable", "guess-yes":
		return true, true
	case "no", "false", "unrestricted", "guess-no":
		return false, true
	default:
		return false, false
	}
}

func (c *Collector) Collect(ctx context.Context, now time.Time, includeSSID bool) protocol.LinkContext {
	local, err := platformContext(ctx, includeSSID)
	if err != nil {
		local = LocalContext{InterfaceType: "unknown"}
	}
	return c.collectLocal(local, now)
}

func (c *Collector) collectLocal(local LocalContext, now time.Time) protocol.LinkContext {
	c.mu.Lock()
	changed := c.lastGateway != "" && local.Gateway != "" && c.lastGateway != local.Gateway
	association := local.InterfaceType + "\x00" + strconv.Itoa(local.FrequencyMHz) + "\x00" + strconv.Itoa(local.Channel) + "\x00" + local.PHY
	associationEvidence := local.ContextAvailable && local.InterfaceType == "wifi" && (local.FrequencyMHz > 0 || local.Channel > 0 || local.PHY != "")
	associationChanged := associationEvidence && c.lastAssociation != "" && association != c.lastAssociation
	if local.Gateway != "" {
		c.lastGateway = local.Gateway
	}
	if associationEvidence {
		c.lastAssociation = association
	}
	c.mu.Unlock()
	return protocol.LinkContext{
		Timestamp: now.Unix(), InterfaceType: normalizeInterfaceType(local.InterfaceType),
		LinkMbps: boundedFloat(local.LinkMbps, 0, 100000), TXLinkMbps: boundedFloat(local.TXLinkMbps, 0, 100000), RXLinkMbps: boundedFloat(local.RXLinkMbps, 0, 100000),
		RSSIDBm: boundedFloat(local.RSSIDBm, -150, 0), NoiseDBm: boundedFloat(local.NoiseDBm, -150, 0), SNRDB: boundedFloat(local.SNRDB, 0, 150),
		FrequencyMHz: min(max(local.FrequencyMHz, 0), 100000), Channel: min(max(local.Channel, 0), 10000), ChannelWidthMHz: min(max(local.ChannelWidthMHz, 0), 1000),
		PHY: truncate(local.PHY, 64), Security: truncate(local.Security, 64),
		SSID:    truncate(local.SSID, 64),
		Metered: local.Metered, OnBattery: local.OnBattery,
		BatteryPct: boundedFloat(local.BatteryPct, 0, 100), GatewayChanged: changed,
		ContextAvailable: local.ContextAvailable, AssociationChanged: associationChanged,
		RawNetworkIdentity: local.NetworkIdentity, RawFirstHop: local.Gateway,
		Availability: protocol.LinkAvailability{
			Interface: local.ContextAvailable && local.InterfaceType != "" && local.InterfaceType != "unknown",
			LinkRate:  local.LinkMbps > 0 || local.TXLinkMbps > 0 || local.RXLinkMbps > 0,
			RSSI:      local.RSSIDBm < 0, Noise: local.NoiseDBm < 0, SNR: local.SNRDB > 0,
			Frequency: local.FrequencyMHz > 0, Channel: local.Channel > 0, ChannelWidth: local.ChannelWidthMHz > 0,
			PHY: local.PHY != "" && local.PHY != "unknown", Security: local.Security != "",
			Power: local.PowerAvailable, Metered: local.MeteredAvailable, Association: associationEvidence,
			NetworkIdentity: local.NetworkIdentity != "", FirstHop: local.Gateway != "",
		},
	}
}

func MeasureCrossTraffic(ctx context.Context, interval time.Duration) (float64, error) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	before, err := totalInterfaceBytes(ctx)
	if err != nil {
		return 0, err
	}
	started := time.Now()
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-timer.C:
	}
	after, err := totalInterfaceBytes(ctx)
	if err != nil {
		return 0, err
	}
	if after < before {
		return 0, errors.New("interface byte counter moved backwards")
	}
	seconds := time.Since(started).Seconds()
	if seconds <= 0 {
		return 0, errors.New("invalid traffic sample interval")
	}
	return float64(after-before) * 8 / seconds / 1_000_000, nil
}

// SpeedWindowDue reports whether the current local-time window has not yet
// received a successful test. Invalid windows are ignored defensively.
func SpeedWindowDue(now, last time.Time, windows []string, seed string) bool {
	for _, window := range windows {
		scheduled, start, ok := speedWindowSchedule(now, window, seed)
		if ok && !now.Before(scheduled) && last.Before(start) {
			return true
		}
	}
	return false
}

// SpeedProfileDue applies an optional bounded cadence inside the profile's
// allowed windows. Windows therefore double as quiet-hour exclusions.
func SpeedProfileDue(now, last time.Time, windows []string, cadenceMinutes int, seed string) bool {
	if cadenceMinutes <= 0 {
		return SpeedWindowDue(now, last, windows, seed)
	}
	start, end := cadenceSlot(now, cadenceMinutes)
	scheduled, ok := cadenceSchedule(start, end, windows, seed)
	return ok && !now.Before(scheduled) && now.Before(end) && last.Before(start)
}

// NextSpeedWindow returns the next stable randomized test time that has not
// already been satisfied. The bounded look-ahead covers overnight windows and
// the following local day without putting schedule parsing in the UI.
func NextSpeedWindow(now, last time.Time, windows []string, seed string) time.Time {
	if SpeedWindowDue(now, last, windows, seed) {
		return now
	}
	var next time.Time
	for hour := -24; hour <= 48; hour++ {
		candidate := now.Add(time.Duration(hour) * time.Hour)
		for _, window := range windows {
			scheduled, start, ok := speedWindowSchedule(candidate, window, seed)
			if !ok || scheduled.Before(now) || !last.Before(start) {
				continue
			}
			if next.IsZero() || scheduled.Before(next) {
				next = scheduled
			}
		}
	}
	return next
}

func NextSpeedProfile(now, last time.Time, windows []string, cadenceMinutes int, seed string) time.Time {
	if cadenceMinutes <= 0 {
		return NextSpeedWindow(now, last, windows, seed)
	}
	if SpeedProfileDue(now, last, windows, cadenceMinutes, seed) {
		return now
	}
	minutes := min(max(cadenceMinutes, 60), 1440)
	midnight := localClock(now, 0)
	var next time.Time
	for day := -1; day <= 3; day++ {
		dayStart := midnight.AddDate(0, 0, day)
		for offset := 0; offset < 1440; offset += minutes {
			start := localClock(dayStart, offset)
			end := localClock(dayStart, min(offset+minutes, 1440))
			scheduled, ok := cadenceSchedule(start, end, windows, seed)
			if !ok || scheduled.Before(now) || !last.Before(start) {
				continue
			}
			if next.IsZero() || scheduled.Before(next) {
				next = scheduled
			}
		}
	}
	return next
}

func cadenceSlot(now time.Time, cadenceMinutes int) (time.Time, time.Time) {
	minutes := min(max(cadenceMinutes, 60), 1440)
	offset := now.Hour()*60 + now.Minute()
	startMinute := offset / minutes * minutes
	return localClock(now, startMinute), localClock(now, min(startMinute+minutes, 1440))
}

func cadenceSchedule(slotStart, slotEnd time.Time, windows []string, seed string) (time.Time, bool) {
	type interval struct{ start, end time.Time }
	intervals := make([]interval, 0, len(windows))
	midnight := localClock(slotStart, 0)
	var total time.Duration
	for day := -1; day <= 1; day++ {
		base := midnight.AddDate(0, 0, day)
		for _, window := range windows {
			parts := strings.Split(window, "-")
			if len(parts) != 2 {
				continue
			}
			startMinutes, startOK := clockMinutes(parts[0])
			endMinutes, endOK := clockMinutes(parts[1])
			if !startOK || !endOK || startMinutes == endMinutes {
				continue
			}
			start := localClock(base, startMinutes)
			end := localClock(base, endMinutes)
			if endMinutes < startMinutes {
				end = localClock(base, endMinutes+1440)
			}
			start, end = maxTime(start, slotStart), minTime(end, slotEnd)
			if !start.Before(end) {
				continue
			}
			intervals = append(intervals, interval{start: start, end: end})
			total += end.Sub(start)
		}
	}
	if total <= 0 {
		return time.Time{}, false
	}
	digest := sha256.Sum256([]byte(seed + "\x00cadence\x00" + slotStart.Format(time.RFC3339)))
	offset := time.Duration(binary.LittleEndian.Uint64(digest[:8]) % uint64(total))
	for _, allowed := range intervals {
		duration := allowed.end.Sub(allowed.start)
		if offset < duration {
			return allowed.start.Add(offset), true
		}
		offset -= duration
	}
	return intervals[len(intervals)-1].start, true
}

func minTime(left, right time.Time) time.Time {
	if left.Before(right) {
		return left
	}
	return right
}

func maxTime(left, right time.Time) time.Time {
	if left.After(right) {
		return left
	}
	return right
}

func speedWindowSchedule(now time.Time, window, seed string) (time.Time, time.Time, bool) {
	parts := strings.Split(window, "-")
	if len(parts) != 2 {
		return time.Time{}, time.Time{}, false
	}
	startMinutes, startOK := clockMinutes(parts[0])
	endMinutes, endOK := clockMinutes(parts[1])
	if !startOK || !endOK || startMinutes == endMinutes {
		return time.Time{}, time.Time{}, false
	}
	start := localClock(now, startMinutes)
	end := localClock(now, endMinutes)
	if startMinutes > endMinutes {
		if now.Before(end) {
			start = localClock(now.AddDate(0, 0, -1), startMinutes)
		} else {
			end = localClock(now, endMinutes+1440)
		}
	}
	if now.Before(start) || !now.Before(end) {
		return time.Time{}, time.Time{}, false
	}
	duration := end.Sub(start)
	digest := sha256.Sum256([]byte(seed + "\x00" + window + "\x00" + start.Format(time.RFC3339)))
	offset := time.Duration(binary.LittleEndian.Uint64(digest[:8]) % uint64(duration))
	return start.Add(offset), start, true
}

// localClock constructs a wall-clock minute on a calendar day instead of
// adding elapsed time to midnight, which shifts configured hours across DST.
// Go deterministically chooses one occurrence for an ambiguous fall-back
// time. For a nonexistent spring-forward time, advance to the first valid
// wall minute so a boundary never escapes into the following quiet period.
func localClock(day time.Time, minuteOfDay int) time.Time {
	dayOffset := minuteOfDay / 1440
	minuteOfDay %= 1440
	if minuteOfDay < 0 {
		minuteOfDay += 1440
		dayOffset--
	}
	base := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, day.Location()).AddDate(0, 0, dayOffset)
	for adjustment := 0; adjustment <= 180; adjustment++ {
		total := minuteOfDay + adjustment
		candidateDay := base.AddDate(0, 0, total/1440)
		hour, minute := (total%1440)/60, total%60
		candidate := time.Date(candidateDay.Year(), candidateDay.Month(), candidateDay.Day(), hour, minute, 0, 0, day.Location())
		if candidate.Year() == candidateDay.Year() && candidate.Month() == candidateDay.Month() && candidate.Day() == candidateDay.Day() && candidate.Hour() == hour && candidate.Minute() == minute {
			return candidate
		}
	}
	return base
}

func clockMinutes(value string) (int, bool) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return 0, false
	}
	hour, hourErr := strconv.Atoi(parts[0])
	minute, minuteErr := strconv.Atoi(parts[1])
	if hourErr != nil || minuteErr != nil || minute < 0 || minute > 59 || hour < 0 || hour > 24 || (hour == 24 && minute != 0) {
		return 0, false
	}
	return hour*60 + minute, true
}

func (r SpeedRunner) RunTest(ctx context.Context, traffic protocol.TrafficContext, remainingBytes uint64, profile protocol.SpeedProfileConfig, household HouseholdRequest) (protocol.SpeedTest, error) {
	if remainingBytes < 1<<20 {
		return protocol.SpeedTest{}, errors.New("daily data budget exhausted")
	}
	if !protocol.KnownSpeedProfile(profile.Name) {
		return protocol.SpeedTest{}, errors.New("unknown speed-test profile")
	}
	if profile.Name == protocol.ProfileHousehold && r.Run != nil {
		return protocol.SpeedTest{}, errors.New("household profile requires the native quality runner")
	}
	requestedTotal := profile.MaxDownloadBytes + profile.MaxUploadBytes
	if requestedTotal == 0 {
		return protocol.SpeedTest{}, errors.New("speed-test profile has no data budget")
	}
	effectiveTotal := min(requestedTotal, remainingBytes)
	downloadBudget := effectiveTotal * profile.MaxDownloadBytes / requestedTotal
	uploadBudget := effectiveTotal - downloadBudget
	if downloadBudget < 512<<10 || uploadBudget < 256<<10 {
		return protocol.SpeedTest{}, errors.New("insufficient daily data budget for speed test")
	}
	budget := quality.Budget{
		RequestedDownloadBytes: profile.MaxDownloadBytes, RequestedUploadBytes: profile.MaxUploadBytes,
		EffectiveDownloadBytes: downloadBudget, EffectiveUploadBytes: uploadBudget,
		MaxDurationSeconds: profile.MaxDurationSeconds, MaxConcurrentRequests: profile.MaxConcurrentRequests,
		PredictedBytes: effectiveTotal,
	}
	run := r.Run
	if run == nil {
		effectiveProfile := profile
		effectiveProfile.MaxDownloadBytes = downloadBudget
		effectiveProfile.MaxUploadBytes = uploadBudget
		result, err := (qualitytest.Runner{}).Run(ctx, qualitytest.Options{Profile: effectiveProfile, Traffic: traffic, OnProgress: r.Progress, HouseholdMix: household.Mix, MixSource: household.Source})
		used, capHit, predicted := result.Budget.UsedBytes, result.Budget.CapHit, result.Budget.PredictedBytes
		result.Budget = budget
		result.Budget.UsedBytes, result.Budget.CapHit = used, capHit
		if predicted > 0 {
			// The runner knows what it planned to spend (a paced household
			// mix is far below its cap); the envelope is only the ceiling.
			result.Budget.PredictedBytes = predicted
		}
		hardenWire(&result)
		return result, err
	}
	started := time.Now()
	result := run(ctx, diagnostics.SpeedTestOptions{
		MaxDurationSeconds: uint32(profile.MaxDurationSeconds), MaxDownloadBytes: downloadBudget, MaxUploadBytes: uploadBudget,
		PingCount: 20, Profile: diagnostics.SpeedTestProfileStandard,
	})
	out := convertSpeedResult(started, result, traffic, profile, budget)
	if err := requiredSpeedEvidence(out); err != nil {
		return out, err
	}
	return out, nil
}

func requiredSpeedEvidence(result protocol.SpeedTest) error {
	switch {
	case result.Download.Error != "":
		return fmt.Errorf("speed test download phase failed: %s", result.Download.Error)
	case result.Upload.Error != "":
		return fmt.Errorf("speed test upload phase failed: %s", result.Upload.Error)
	case !throughputAvailable(result.Download):
		return errors.New("speed test download phase produced no usable throughput")
	case !throughputAvailable(result.Upload):
		return errors.New("speed test upload phase produced no usable throughput")
	case result.Responsiveness.Baseline.Count == 0 || result.Responsiveness.DownloadLoaded.Count == 0 || result.Responsiveness.UploadLoaded.Count == 0:
		return errors.New("speed test produced incomplete latency evidence")
	default:
		return nil
	}
}

func convertSpeedResult(started time.Time, in diagnostics.SpeedTestResult, traffic protocol.TrafficContext, profile protocol.SpeedProfileConfig, budget quality.Budget) protocol.SpeedTest {
	response := in.Responsiveness
	out := protocol.SpeedTest{
		MeasurementID: newMeasurementID(), Profile: profile.Name, Timestamp: started.Unix(), Download: convertThroughput(in.Download), Upload: convertThroughput(in.Upload),
		Latency: convertLatency(in.Latency), EndpointHost: truncate(in.EndpointHost, 255),
		CPUBusyPct: boundedFloat(in.CPUBusyPct, 0, 100), DurationMS: boundedFloat(float64(in.DurationMS), 0, 300000),
		TrafficContext: traffic,
		Budget:         budget,
		Responsiveness: protocol.Responsiveness{
			Baseline: convertSummary(response.Baseline), DownloadLoaded: convertSummary(response.DownloadLoaded),
			UploadLoaded: convertSummary(response.UploadLoaded), DownloadBloatP90MS: boundedFloat(response.DownloadBloatP90MS, 0, 60000),
			UploadBloatP90MS: boundedFloat(response.UploadBloatP90MS, 0, 60000), BidirectionalBloatP90MS: boundedFloat(response.BidirectionalBloatP90MS, 0, 60000),
			DownloadGrade: grade(response.DownloadGrade), UploadGrade: grade(response.UploadGrade), BidirectionalGrade: grade(response.BidirectionalGrade), OverallGrade: grade(response.OverallGrade),
			PrimaryDriver: wirePrimaryDriver(response.PrimaryDriver), ConfidenceLevel: confidence(response.ConfidenceLevel),
			ConfidenceReasons: boundedReasons(response.ConfidenceReasons), Profile: speedProfile(response.Profile),
			BidirectionalCountsTowardOverall: response.BidirectionalCountsTowardOverall,
			TotalDownloadBytes:               min(response.TotalDownloadBytes, uint64(2<<30)), TotalUploadBytes: min(response.TotalUploadBytes, uint64(2<<30)),
		},
	}
	if response.Profile != string(diagnostics.SpeedTestProfileQuick) {
		bidirectional := convertSummary(response.BidirectionalLoaded)
		cooldown := convertSummary(response.Cooldown)
		download := convertThroughput(response.BidirectionalDownload)
		upload := convertThroughput(response.BidirectionalUpload)
		out.Responsiveness.BidirectionalLoaded = &bidirectional
		out.Responsiveness.Cooldown = &cooldown
		out.Responsiveness.BidirectionalDownload = &download
		out.Responsiveness.BidirectionalUpload = &upload
	}
	out.Budget.UsedBytes = out.Responsiveness.TotalDownloadBytes + out.Responsiveness.TotalUploadBytes
	if out.Budget.UsedBytes == 0 {
		out.Budget.UsedBytes = out.Download.BytesTransferred + out.Upload.BytesTransferred
	}
	out.Budget.CapHit = out.Download.CapHit || out.Upload.CapHit || out.Budget.UsedBytes >= out.Budget.EffectiveDownloadBytes+out.Budget.EffectiveUploadBytes
	out.Phases = phaseEvidence(out, profile)
	out.Experience = quality.ScoreApplications(quality.ApplicationInput{
		Baseline:        phaseMetrics(&out.Responsiveness.Baseline),
		Download:        phaseMetrics(&out.Responsiveness.DownloadLoaded),
		Upload:          phaseMetrics(&out.Responsiveness.UploadLoaded),
		Bidirectional:   phaseMetrics(out.Responsiveness.BidirectionalLoaded),
		DownloadP75Mbps: representativeThroughput(out.Download), UploadP75Mbps: representativeThroughput(out.Upload),
		DownloadAvailable: throughputAvailable(out.Download), UploadAvailable: throughputAvailable(out.Upload),
	})
	out.ExperienceEligible = profile.CountsTowardExperience
	persistent := out.Responsiveness.BidirectionalLoaded
	if persistent == nil {
		persistent = &out.Responsiveness.DownloadLoaded
	}
	out.Paths.Persistent = pathLatency(persistent)
	out.MeasurementConfidence = quality.GradeConfidence(quality.ConfidenceInput{
		BaselineSamples: int(out.Responsiveness.Baseline.Count),
		PhaseSamples: map[string]int{
			"download": int(out.Responsiveness.DownloadLoaded.Count), "upload": int(out.Responsiveness.UploadLoaded.Count),
			"bidirectional": latencyCount(out.Responsiveness.BidirectionalLoaded),
		},
		StablePhases: map[string]bool{
			"download":      out.Phases["download"].StableRegion.Stable,
			"upload":        out.Phases["upload"].StableRegion.Stable,
			"bidirectional": out.Phases["bidirectional"].StableRegion.Stable,
		},
		ProbeSuccessRatio: probeSuccessRatio(out), AgentCPUBusyPct: out.CPUBusyPct,
	})
	if traffic.Contaminated {
		out.MeasurementConfidence.Level = minConfidence(out.MeasurementConfidence.Level, "medium")
		out.MeasurementConfidence.Reasons = append(out.MeasurementConfidence.Reasons, "cross_traffic_detected")
	}
	out.Responsiveness.ConfidenceLevel = out.MeasurementConfidence.Level
	out.Responsiveness.ConfidenceReasons = append([]string(nil), out.MeasurementConfidence.Reasons...)
	return out
}

func phaseEvidence(result protocol.SpeedTest, profile protocol.SpeedProfileConfig) map[string]protocol.PhaseEvidence {
	minimum := int64(profile.MinDurationSeconds) * 1000
	maximum := int64(profile.MaxDurationSeconds) * 1000
	evidence := map[string]protocol.PhaseEvidence{
		"download": aggregatePhaseEvidence(result.Download, minimum, maximum, profile.MaxConcurrentRequests),
		"upload":   aggregatePhaseEvidence(result.Upload, minimum, maximum, profile.MaxConcurrentRequests),
	}
	if result.Responsiveness.BidirectionalDownload != nil && result.Responsiveness.BidirectionalUpload != nil {
		combined := protocol.ThroughputResult{
			BytesTransferred: result.Responsiveness.BidirectionalDownload.BytesTransferred + result.Responsiveness.BidirectionalUpload.BytesTransferred,
			DurationMS:       max(result.Responsiveness.BidirectionalDownload.DurationMS, result.Responsiveness.BidirectionalUpload.DurationMS),
		}
		evidence["bidirectional"] = aggregatePhaseEvidence(combined, minimum, maximum, min(profile.MaxConcurrentRequests, 4))
	} else {
		evidence["bidirectional"] = protocol.PhaseEvidence{MinDurationMS: minimum, MaxDurationMS: maximum, StableRegion: quality.StableRegion{Reason: "phase_unavailable"}}
	}
	return evidence
}

// The shared v0.4 transport returns only whole-phase totals. Represent that as
// one honest aggregate bucket and lower confidence rather than inventing a
// stable interval. The FOSS-safe stability engine consumes native one-second
// buckets as the transport contract evolves.
func aggregatePhaseEvidence(result protocol.ThroughputResult, minimum, maximum int64, streams int) protocol.PhaseEvidence {
	duration := int64(result.DurationMS)
	bucket := quality.Bucket{Index: 0, DurationMS: duration, Bytes: result.BytesTransferred, Samples: 1, StreamCount: streams, Complete: duration >= 900}
	region := quality.DetectStableRegion([]quality.Bucket{bucket}, quality.DefaultStabilityPolicy())
	return protocol.PhaseEvidence{MinDurationMS: minimum, MaxDurationMS: maximum, EndMS: duration, Buckets: []quality.Bucket{bucket}, StableRegion: region}
}

func phaseMetrics(summary *protocol.LatencySummary) quality.PhaseMetrics {
	if summary == nil {
		return quality.PhaseMetrics{}
	}
	return quality.PhaseMetrics{P95LatencyMS: summary.P95MS, LossPct: summary.PacketLossPct, LatencySamples: int(summary.Count), LossAvailable: summary.PacketLossValid}
}

func pathLatency(summary *protocol.LatencySummary) protocol.PathLatency {
	if summary == nil {
		return protocol.PathLatency{}
	}
	samples := int(summary.Attempts)
	if samples == 0 {
		samples = int(summary.Count)
	}
	successes := samples
	if summary.PacketLossValid {
		successes = int(math.Round(float64(samples) * (1 - summary.PacketLossPct/100)))
	}
	return protocol.PathLatency{Samples: samples, Successes: max(successes, 0), P50MS: summary.P50MS, P95MS: summary.P95MS, LossPct: summary.PacketLossPct}
}

func latencyCount(summary *protocol.LatencySummary) int {
	if summary == nil {
		return 0
	}
	return int(summary.Count)
}

func probeSuccessRatio(result protocol.SpeedTest) float64 {
	count := 0
	success := 0.0
	for _, summary := range []*protocol.LatencySummary{&result.Responsiveness.Baseline, &result.Responsiveness.DownloadLoaded, &result.Responsiveness.UploadLoaded, result.Responsiveness.BidirectionalLoaded} {
		if summary == nil || summary.Count == 0 {
			continue
		}
		count++
		ratio := 1.0
		if summary.PacketLossValid {
			ratio = max(0, 1-summary.PacketLossPct/100)
		}
		success += ratio
	}
	if count == 0 {
		return 0
	}
	return success / float64(count)
}

func newMeasurementID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("m-%d", time.Now().UnixNano())
	}
	return "m-" + hex.EncodeToString(raw[:])
}

func minConfidence(left, right string) string {
	rank := map[string]int{"high": 0, "medium": 1, "low": 2}
	if rank[right] > rank[left] {
		return right
	}
	return left
}

func convertThroughput(in diagnostics.ThroughputResult) protocol.ThroughputResult {
	throughput := boundedFloat(in.ThroughputMbps, 0, 10000)
	return protocol.ThroughputResult{BytesTransferred: min(in.BytesTransferred, uint64(1<<30)), DurationMS: boundedFloat(float64(in.DurationMS), 0, 60000), ThroughputMbps: throughput, P75Mbps: throughput, CapHit: in.CapHit, TTFBMS: boundedFloat(float64(in.TTFBMS), 0, 60000), Error: truncate(in.Error, 512)}
}

func representativeThroughput(result protocol.ThroughputResult) float64 {
	if result.P75Mbps > 0 {
		return result.P75Mbps
	}
	return result.ThroughputMbps
}

func throughputAvailable(result protocol.ThroughputResult) bool {
	return result.BytesTransferred > 0 && representativeThroughput(result) > 0
}

func convertLatency(in diagnostics.LatencyResult) protocol.LatencyResult {
	return protocol.LatencyResult{MinMS: boundedFloat(in.MinMS, 0, 60000), AvgMS: boundedFloat(in.AvgMS, 0, 60000), MaxMS: boundedFloat(in.MaxMS, 0, 60000), JitterMS: boundedFloat(in.JitterMS, 0, 60000), PacketLossPct: boundedFloat(in.PacketLossPct, 0, 100), PacketLossValid: in.PacketLossValid, ProbesSent: min(in.ProbesSent, 1000), LoadedAvgMS: boundedFloat(in.LoadedAvgMS, 0, 60000), Target: truncate(in.Target, 255), Error: truncate(in.Error, 512)}
}

func convertSummary(in diagnostics.LatencySummary) protocol.LatencySummary {
	// The shared transport reports Count as successful samples with loss kept
	// separately, while Attempts means total probes including losses. Recover
	// attempts from count and loss so downstream success math is not
	// discounted twice.
	attempts := in.Count
	if in.PacketLossValid && in.PacketLossPct > 0 && in.PacketLossPct < 100 {
		// Cap the estimate before the uint32 conversion: loss just under 100%
		// makes the divisor tiny and the float estimate can exceed MaxUint32.
		attempts = uint32(min(math.Round(float64(in.Count)/(1-in.PacketLossPct/100)), 1000))
	}
	return protocol.LatencySummary{Count: min(in.Count, 1000), Attempts: min(attempts, 1000), MinMS: boundedFloat(in.MinMS, 0, 60000), P5MS: boundedFloat(in.P5MS, 0, 60000), P50MS: boundedFloat(in.P50MS, 0, 60000), P90MS: boundedFloat(in.P90MS, 0, 60000), P95MS: boundedFloat(in.P95MS, 0, 60000), P99MS: boundedFloat(in.P99MS, 0, 60000), MaxMS: boundedFloat(in.MaxMS, 0, 60000), JitterIQRMS: boundedFloat(in.JitterIQRMS, 0, 60000), JitterMeanDeltaMS: boundedFloat(in.JitterMeanDeltaMS, 0, 60000), PacketLossPct: boundedFloat(in.PacketLossPct, 0, 100), PacketLossValid: in.PacketLossValid, Error: truncate(in.Error, 512)}
}

func boundedFloat(value, low, high float64) float64 {
	if value != value || value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func normalizeInterfaceType(value string) string {
	switch value {
	case "wifi", "ethernet", "cellular", "other":
		return value
	default:
		return "unknown"
	}
}

func grade(value string) string {
	switch value {
	case "A+", "A", "B", "C", "D", "F":
		return value
	default:
		return ""
	}
}

func confidence(value string) string {
	switch value {
	case "high", "medium", "low":
		return value
	default:
		return "low"
	}
}

func speedProfile(value string) string {
	switch value {
	case "standard", "scenario":
		return value
	default:
		return "quick"
	}
}

// hardenWire pins a runner result to the ingest contract the controller
// enforces strictly. A nil reason slice encodes as JSON null where the
// controller requires a list, and a primary driver outside download/upload
// is not a value it accepts; either mistake makes it drop the whole record
// silently after acknowledging the batch. The household driver is conveyed
// by the household block itself, so the wire carries no driver for it.
func hardenWire(result *protocol.SpeedTest) {
	if result.Responsiveness.ConfidenceReasons == nil {
		result.Responsiveness.ConfidenceReasons = []string{}
	}
	if result.MeasurementConfidence.Reasons == nil {
		result.MeasurementConfidence.Reasons = []string{}
	}
	switch result.Responsiveness.PrimaryDriver {
	case "", "download", "upload":
	default:
		result.Responsiveness.PrimaryDriver = ""
	}
}

func wirePrimaryDriver(value string) string {
	if value == "upload_bufferbloat" {
		return "upload"
	}
	if value == "download_bufferbloat" {
		return "download"
	}
	return ""
}

func boundedReasons(values []string) []string {
	if len(values) > 32 {
		values = values[:32]
	}
	out := make([]string, len(values))
	for index, value := range values {
		out[index] = truncate(value, 128)
	}
	return out
}

func truncate(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	return value[:maximum]
}
