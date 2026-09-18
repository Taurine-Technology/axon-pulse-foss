package qualitytest

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Taurine-Technology/axon-pulse/internal/protocol"
	"github.com/Taurine-Technology/axon-pulse/quality"
)

// The household profile reproduces a configured mix of everyday activities,
// each paced to its own reference demand on its own HTTP/1.1 connections, and
// scores every activity from its own delivery requirements. It never drives
// the link to capacity: that is the saturation profile's job.

const (
	// householdMaxSamples bounds per-instance memory; the busiest instance
	// (a game probing every 50 ms) produces well under this in a 30 s run.
	householdMaxSamples = 4096
	// householdMaxConnections bounds the shared transfer pool regardless of
	// the mix, so the sensor's socket footprint stays predictable.
	householdMaxConnections = 32
	// householdCooldownReserve is wall-clock kept back for the cooldown
	// probes when planning the scenario window.
	householdCooldownReserve = 1500 * time.Millisecond
	// householdProbeReserve is the payload reserved for latency and game
	// probes out of the per-test cap.
	householdProbeReserve = 1 << 20
	// householdRequestTimeout bounds one delivery unit; a unit slower than
	// this is a failure for every activity in the catalog.
	householdRequestTimeout = 5 * time.Second
)

type (
	householdLedger struct {
		cap       uint64
		remaining atomic.Uint64
		download  atomic.Uint64
		upload    atomic.Uint64
		exhausted atomic.Bool
	}

	householdInstance struct {
		profile quality.ActivityProfile
		index   int
		mu      sync.Mutex
		samples []quality.ActivitySample
	}

	householdRun struct {
		transfer    *http.Client
		cold        *http.Client
		downloadURL string
		uploadURL   string
		ledger      *householdLedger
		responses   *responseBudget
		started     time.Time
		down        *phaseCounter
		up          *phaseCounter
	}

	householdTransfer struct {
		bytes       uint64
		firstByteMS int64
		err         error
	}
)

func newHouseholdLedger(capBytes uint64) *householdLedger {
	ledger := &householdLedger{cap: capBytes}
	ledger.remaining.Store(uint64(float64(capBytes) * (1 - quality.HouseholdOverheadReserve)))
	return ledger
}

// claim reserves a whole delivery unit or nothing. Partial units would let
// an activity quietly run below its demand, which the product forbids.
func (l *householdLedger) claim(amount uint64) bool {
	for {
		current := l.remaining.Load()
		if current < amount {
			l.exhausted.Store(true)
			return false
		}
		if l.remaining.CompareAndSwap(current, current-amount) {
			return true
		}
	}
}

func (l *householdLedger) settle(claimed, moved uint64, upload bool) {
	if moved < claimed {
		l.remaining.Add(claimed - moved)
	}
	if upload {
		l.upload.Add(moved)
	} else {
		l.download.Add(moved)
	}
}

func (i *householdInstance) record(sample quality.ActivitySample) {
	i.mu.Lock()
	if len(i.samples) < householdMaxSamples {
		i.samples = append(i.samples, sample)
	}
	i.mu.Unlock()
}

func (i *householdInstance) snapshot() []quality.ActivitySample {
	i.mu.Lock()
	defer i.mu.Unlock()
	return append([]quality.ActivitySample(nil), i.samples...)
}

// householdClient forces HTTP/1.1 so every in-flight unit rides its own TCP
// connection. HTTP/2 would multiplex the whole household onto one connection
// and let a 4K segment head-of-line block the call frames, which no real
// household experiences.
func householdClient(base *http.Client, fresh bool, concurrency int) *http.Client {
	cloned := cloneClient(base, fresh, concurrency)
	if transport, ok := cloned.Transport.(*http.Transport); ok {
		transport.ForceAttemptHTTP2 = false
		transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	return cloned
}

func (Runner) runHousehold(ctx context.Context, options Options) (protocol.SpeedTest, error) {
	profile := options.Profile
	if profile.MaxDownloadBytes < 512<<10 || profile.MaxUploadBytes < 256<<10 {
		return protocol.SpeedTest{}, errors.New("household test byte budget is too small")
	}
	if profile.MaxDurationSeconds < 8 || profile.MaxDurationSeconds > 120 {
		return protocol.SpeedTest{}, errors.New("household test duration is outside local bounds")
	}
	mix, source := quality.NormalizeHouseholdMix(options.HouseholdMix), defaultString(options.MixSource, quality.MixSourceDefault)
	started := now(options.Now)
	measurementID := measurementID(started)
	downloadURL := defaultString(options.DownloadURL, DefaultDownloadURL)
	uploadURL := defaultString(options.UploadURL, DefaultUploadURL)
	if err := validateEndpoint(downloadURL, options.Client != nil); err != nil {
		return protocol.SpeedTest{}, fmt.Errorf("download endpoint: %w", err)
	}
	if err := validateEndpoint(uploadURL, options.Client != nil); err != nil {
		return protocol.SpeedTest{}, fmt.Errorf("upload endpoint: %w", err)
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(profile.MaxDurationSeconds)*time.Second)
	defer cancel()

	activities := mix.Activities()
	concurrency := 2
	for _, activity := range activities {
		directions := 1
		if activity.UpMbps > 0 && activity.Pattern == quality.PatternFrames {
			directions = 2
		}
		concurrency += activity.InFlight * directions
	}
	concurrency = min(concurrency, householdMaxConnections)
	capBytes := profile.MaxDownloadBytes + profile.MaxUploadBytes
	ledger := newHouseholdLedger(capBytes)
	probeReserve := min(uint64(householdProbeReserve), capBytes/100)
	if !ledger.claim(probeReserve) {
		probeReserve = 0
	}
	run := &householdRun{
		transfer: householdClient(options.Client, false, concurrency), cold: householdClient(options.Client, true, 1),
		downloadURL: downloadURL, uploadURL: uploadURL, ledger: ledger,
		responses: newResponseBudget(probeReserve), started: started,
	}
	persistentClient := cloneClient(options.Client, false, 1)
	defer run.transfer.CloseIdleConnections()
	defer run.cold.CloseIdleConnections()
	defer persistentClient.CloseIdleConnections()

	result := protocol.SpeedTest{
		MeasurementID: measurementID, Profile: profile.Name, Timestamp: started.Unix(),
		EndpointHost: endpointHost(downloadURL), TrafficContext: options.Traffic,
		ExperienceEligible: profile.CountsTowardExperience,
		Budget: quality.Budget{
			RequestedDownloadBytes: profile.MaxDownloadBytes, RequestedUploadBytes: profile.MaxUploadBytes,
			EffectiveDownloadBytes: profile.MaxDownloadBytes, EffectiveUploadBytes: profile.MaxUploadBytes,
			MaxDurationSeconds: profile.MaxDurationSeconds, MaxConcurrentRequests: profile.MaxConcurrentRequests,
		},
		Phases: map[string]protocol.PhaseEvidence{},
	}
	household := quality.HouseholdResult{
		Transport: quality.HouseholdTransportHTTPS, Mix: mix, MixSource: source,
		Ledger: quality.HouseholdLedger{CapBytes: capBytes},
	}

	progress := newProgressEmitter(options.OnProgress, measurementID, profile.Name, started)
	progress.phase("baseline")
	baselineStart := elapsedMS(started)
	baseline, baselinePaths := collectBaselineLatency(runCtx, persistentClient, downloadURL, run.responses, progress.rtt)
	result.Latency = compatibilityLatency(baseline, endpointHost(downloadURL))
	result.Responsiveness.Baseline = baseline
	result.Phases["baseline"] = latencyEvidence(baselineStart, elapsedMS(started), "latency_only_phase")
	progress.baseline(baseline.P50MS)

	deadline := started.Add(time.Duration(profile.MaxDurationSeconds) * time.Second)
	available := time.Until(deadline) - householdCooldownReserve
	plan := quality.PlanHousehold(mix, capBytes, available.Milliseconds())
	household.Ledger.PlannedBytes = plan.PlannedBytes
	household.Window.WarmupMS, household.Window.PlannedMS = plan.WarmupMS, plan.ObserveMS
	result.Budget.PredictedBytes = plan.PlannedBytes + probeReserve

	if plan.Reference && runCtx.Err() == nil {
		if reference, ok := mix.ReferenceActivity(); ok {
			progress.phase("reference")
			household.Reference = run.runReference(runCtx, reference, plan.ReferenceMS, progress)
			result.Phases["reference"] = run.phaseEvidence(started, "paced_scenario")
		}
	}

	progress.phase("household")
	stopReason := quality.StopEnoughEvidence
	windowStart, windowEnd := int64(0), int64(0)
	var loaded protocol.LatencySummary
	var loadedPaths pathSamples
	instances := make([]*householdInstance, len(activities))
	offsets := quality.StaggerOffsets(activities)
	if runCtx.Err() == nil {
		run.down, run.up = newPhaseCounter(len(activities)), newPhaseCounter(len(activities))
		phaseStart := time.Now()
		phaseMS := elapsedMS(started)
		phaseCtx, stop := context.WithTimeout(runCtx, time.Duration(plan.WarmupMS+plan.ObserveMS)*time.Millisecond)
		var wait sync.WaitGroup
		latencyDone := make(chan protocol.LatencySummary, 1)
		go func() {
			latencyDone <- collectLoadedLatency(phaseCtx, persistentClient, endpointForProbe(downloadURL), &loadedPaths, run.responses, progress.rtt)
		}()
		for index, activity := range activities {
			instance := &householdInstance{profile: activity, index: index + 1, samples: make([]quality.ActivitySample, 0, 256)}
			instances[index] = instance
			start := phaseStart.Add(time.Duration(offsets[index]) * time.Millisecond)
			wait.Go(func() { run.runInstance(phaseCtx, instance, start) })
		}
		ticker := time.NewTicker(250 * time.Millisecond)
		exhausted := false
	watch:
		for {
			select {
			case <-ticker.C:
				progress.rates(ledger.download.Load(), ledger.upload.Load())
				if ledger.exhausted.Load() {
					exhausted = true
					stop()
					break watch
				}
			case <-phaseCtx.Done():
				break watch
			}
		}
		ticker.Stop()
		wait.Wait()
		stop()
		loaded = <-latencyDone
		phaseEndMS := elapsedMS(started)
		windowStart = phaseMS + plan.WarmupMS
		windowEnd = min(phaseEndMS, phaseMS+plan.WarmupMS+plan.ObserveMS)
		household.Window.StartMS, household.Window.EndMS = phaseMS, phaseEndMS
		switch {
		case exhausted:
			stopReason = quality.StopInsufficientBytes
		case ctx.Err() != nil:
			stopReason = quality.StopCancelled
		case runCtx.Err() != nil:
			stopReason = quality.StopInsufficientTime
		case !plan.Sufficient:
			// The plan already knew the shortest window could not be
			// funded; whatever the run happened to observe is not a verdict.
			stopReason = plan.Reason
		}
		result.Phases["household"] = protocol.PhaseEvidence{
			MinDurationMS: plan.WarmupMS + quality.HouseholdMinObserveMS, MaxDurationMS: plan.WarmupMS + plan.ObserveMS,
			StartMS: phaseMS, EndMS: phaseEndMS,
			Buckets:      combineBuckets(run.down.snapshot(true), run.up.snapshot(true), len(activities)),
			StableRegion: quality.StableRegion{Reason: "paced_scenario"},
		}
	} else {
		stopReason = quality.StopInsufficientTime
		if ctx.Err() != nil {
			stopReason = quality.StopCancelled
		}
	}
	household.Window.ObservedMS = max(windowEnd-windowStart, 0)

	var observedDown, observedUp uint64
	successes := 0
	household.Activities = make([]quality.HouseholdActivity, 0, len(instances))
	for _, instance := range instances {
		if instance == nil {
			continue
		}
		samples := instance.snapshot()
		for _, sample := range samples {
			if sample.Failed || sample.ScheduledMS < windowStart || sample.ScheduledMS >= windowEnd {
				continue
			}
			successes++
			if sample.Upload {
				observedUp += sample.Bytes
			} else {
				observedDown += sample.Bytes
			}
		}
		household.Activities = append(household.Activities, quality.SummarizeActivity(instance.profile, instance.index, samples, windowStart, windowEnd))
	}
	if successes == 0 && baseline.Count == 0 {
		stopReason = quality.StopTransportFailure
	}
	household.StopReason = stopReason

	progress.phase("cooldown")
	cooldownStart := elapsedMS(started)
	cooldown, cooldownPaths := collectFixedLatency(runCtx, persistentClient, downloadURL, 6, run.responses, progress.rtt)
	result.Responsiveness.Cooldown = &cooldown
	result.Phases["cooldown"] = latencyEvidence(cooldownStart, elapsedMS(started), "latency_only_phase")

	observedSeconds := float64(household.Window.ObservedMS) / 1000
	result.Download = aggregateThroughput(observedDown, observedSeconds)
	result.Upload = aggregateThroughput(observedUp, observedSeconds)
	if loaded.Attempts > 0 {
		result.Responsiveness.BidirectionalLoaded = &loaded
		result.Responsiveness.BidirectionalBloatP90MS, result.Responsiveness.BidirectionalGrade = bloat(baseline, loaded)
		result.Responsiveness.OverallGrade = result.Responsiveness.BidirectionalGrade
		result.Responsiveness.PrimaryDriver = "household"
	}
	result.Responsiveness.Profile = protocol.ProfileHousehold
	household.Ledger.DownloadBytes = ledger.download.Load()
	household.Ledger.UploadBytes = ledger.upload.Load()
	household.Ledger.ProbeBytes = run.responses.used.Load()
	household.Ledger.UsedBytes = household.Ledger.DownloadBytes + household.Ledger.UploadBytes + household.Ledger.ProbeBytes
	result.Responsiveness.TotalDownloadBytes = household.Ledger.DownloadBytes + household.Ledger.ProbeBytes
	result.Responsiveness.TotalUploadBytes = household.Ledger.UploadBytes
	result.Budget.UsedBytes = household.Ledger.UsedBytes
	result.Budget.CapHit = ledger.exhausted.Load()

	quality.SummarizeHousehold(&household)
	// Name the link's own spikes when it has them: an idle p95 far above the
	// idle median (typically Wi-Fi) is what a struggling real-time activity
	// is seeing, and the mix did not cause it.
	if household.Status != quality.HouseholdStatusPass && household.Status != quality.HouseholdStatusInsufficient && baseline.Count >= quality.MinimumBaselineSamples && baseline.P95MS-baseline.P50MS > 100 {
		household.Summary += fmt.Sprintf(" Your connection already shows latency spikes when idle (p95 %.0f ms against a typical %.0f ms), usually a Wi-Fi symptom; the mix did not create them.", baseline.P95MS, baseline.P50MS)
	}
	result.Household = &household
	result.Experience = quality.HouseholdExperience(household)

	allPaths := baselinePaths
	allPaths.merge(loadedPaths)
	allPaths.merge(cooldownPaths)
	result.Paths = allPaths.summary()
	unavailable := 0
	for _, activity := range household.Activities {
		if !activity.Available {
			unavailable++
		}
	}
	result.MeasurementConfidence = quality.HouseholdConfidence(quality.HouseholdConfidenceInput{
		BaselineSamples: int(baseline.Count), LoadedSamples: int(loaded.Count),
		ProbeSuccessRatio: allPaths.successRatio(), FreshPathSuccessRatio: allPaths.freshSuccessRatio(),
		ObservedMS: household.Window.ObservedMS, PlannedMS: plan.ObserveMS, StopReason: stopReason,
		UnavailableActivities: unavailable,
	})
	if options.Traffic.Contaminated {
		result.MeasurementConfidence.Level = lowerConfidence(result.MeasurementConfidence.Level, "medium")
		result.MeasurementConfidence.Reasons = append(result.MeasurementConfidence.Reasons, "cross_traffic_detected")
	}
	result.Responsiveness.ConfidenceLevel = result.MeasurementConfidence.Level
	result.Responsiveness.ConfidenceReasons = append([]string(nil), result.MeasurementConfidence.Reasons...)
	result.DurationMS = float64(time.Since(started).Milliseconds())
	progress.done(result)
	switch {
	case ctx.Err() != nil:
		return result, fmt.Errorf("household test aborted: %w", ctx.Err())
	case stopReason == quality.StopTransportFailure:
		return result, errors.New("household test could not reach the measurement endpoint")
	}
	return result, nil
}

func aggregateThroughput(bytes uint64, seconds float64) protocol.ThroughputResult {
	result := protocol.ThroughputResult{BytesTransferred: bytes, DurationMS: seconds * 1000}
	if seconds > 0 {
		result.ThroughputMbps = round(float64(bytes)*8/seconds/1_000_000, 3)
		result.P75Mbps = result.ThroughputMbps
	}
	return result
}

// runReference measures the highest-demand stream alone so the mix result can
// say whether the activity works at all before it says whether it coexists.
func (r *householdRun) runReference(ctx context.Context, profile quality.ActivityProfile, durationMS int64, progress *progressEmitter) *quality.HouseholdActivity {
	r.down, r.up = newPhaseCounter(1), newPhaseCounter(1)
	instance := &householdInstance{profile: profile, index: 1, samples: make([]quality.ActivitySample, 0, 8)}
	phaseCtx, stop := context.WithTimeout(ctx, time.Duration(durationMS)*time.Millisecond)
	defer stop()
	startMS := elapsedMS(r.started)
	phaseStart := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.runInstance(phaseCtx, instance, phaseStart)
	}()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for waiting := true; waiting; {
		select {
		case <-ticker.C:
			progress.rates(r.ledger.download.Load(), r.ledger.upload.Load())
		case <-done:
			waiting = false
		}
	}
	endMS := elapsedMS(r.started)
	// The first segment is startup; the window starts once it is due again.
	activity := quality.SummarizeActivity(profile, 1, instance.snapshot(), startMS+profile.IntervalMS, endMS)
	return &activity
}

func (r *householdRun) phaseEvidence(started time.Time, reason string) protocol.PhaseEvidence {
	buckets := combineBuckets(r.down.snapshot(true), r.up.snapshot(true), 1)
	evidence := protocol.PhaseEvidence{Buckets: buckets, StableRegion: quality.StableRegion{Reason: reason}}
	if len(buckets) > 0 {
		evidence.StartMS = elapsedMS(started) - buckets[len(buckets)-1].StartMS - buckets[len(buckets)-1].DurationMS
		evidence.EndMS = elapsedMS(started)
	}
	return evidence
}

func (r *householdRun) ms() int64 { return elapsedMS(r.started) }

// runInstance drives one activity instance until ctx ends.
func (r *householdRun) runInstance(ctx context.Context, instance *householdInstance, phaseStart time.Time) {
	switch instance.profile.Pattern {
	case quality.PatternSegments:
		r.runSegments(ctx, instance, phaseStart)
	case quality.PatternFrames:
		r.runFrames(ctx, instance, phaseStart)
	case quality.PatternProbes:
		r.runProbes(ctx, instance)
	case quality.PatternBursts:
		r.runBursts(ctx, instance)
	}
}

// runSegments fetches one segment per interval. A late segment is fetched
// back to back with the next, the way a draining player buffer behaves.
func (r *householdRun) runSegments(ctx context.Context, instance *householdInstance, phaseStart time.Time) {
	profile := instance.profile
	interval := time.Duration(profile.IntervalMS) * time.Millisecond
	unit := profile.UnitBytes(false)
	scheduled := phaseStart
	for ctx.Err() == nil {
		if !sleepUntil(ctx, scheduled) {
			return
		}
		if !r.ledger.claim(unit) {
			return
		}
		scheduledMS := r.ms() - time.Since(scheduled).Milliseconds()
		startedMS := r.ms()
		transfer := r.transfer1(ctx, r.transfer, false, unit)
		r.ledger.settle(unit, transfer.bytes, false)
		if ctx.Err() != nil {
			return
		}
		completed := time.Now()
		instance.record(quality.ActivitySample{
			ScheduledMS: scheduledMS, StartedMS: startedMS, CompletedMS: r.ms(), FirstByteMS: transfer.firstByteMS,
			Bytes: transfer.bytes, Failed: transfer.err != nil || transfer.bytes < unit,
		})
		scheduled = scheduled.Add(interval)
		if completed.After(scheduled) {
			scheduled = completed
		}
	}
}

// runFrames sends a media-sized frame every interval in each direction with
// a bounded number in flight; a frame that cannot start on time is dropped.
func (r *householdRun) runFrames(ctx context.Context, instance *householdInstance, phaseStart time.Time) {
	profile := instance.profile
	interval := time.Duration(profile.IntervalMS) * time.Millisecond
	directions := []bool{false}
	if profile.UpMbps > 0 {
		directions = append(directions, true)
	}
	slots := map[bool]chan struct{}{}
	for _, upload := range directions {
		slots[upload] = make(chan struct{}, max(profile.InFlight, 1))
	}
	var wait sync.WaitGroup
	defer wait.Wait()
	if !sleepUntil(ctx, phaseStart) {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	phaseStartMS := r.ms() - time.Since(phaseStart).Milliseconds()
	slot := 0
	for ctx.Err() == nil {
		// Frames are due on an absolute cadence from the phase start, so
		// scheduler lag and pool queueing show up as delivery delay rather
		// than vanish. Slots the ticker skipped while this goroutine was
		// descheduled are frames that never started: record them dropped.
		due := int(time.Since(phaseStart) / interval)
		for ; slot < due; slot++ {
			missedMS := phaseStartMS + int64(slot)*profile.IntervalMS
			for _, upload := range directions {
				instance.record(quality.ActivitySample{ScheduledMS: missedMS, StartedMS: missedMS, CompletedMS: missedMS, Upload: upload, Failed: true})
			}
		}
		scheduledMS := phaseStartMS + int64(slot)*profile.IntervalMS
		slot++
		for _, upload := range directions {
			unit := profile.UnitBytes(upload)
			select {
			case slots[upload] <- struct{}{}:
			default:
				instance.record(quality.ActivitySample{ScheduledMS: scheduledMS, StartedMS: scheduledMS, CompletedMS: scheduledMS, Upload: upload, Failed: true})
				continue
			}
			if !r.ledger.claim(unit) {
				<-slots[upload]
				return
			}
			wait.Add(1)
			go func(upload bool, unit uint64, scheduledMS int64) {
				defer wait.Done()
				defer func() { <-slots[upload] }()
				startedMS := r.ms()
				transfer := r.transfer1(ctx, r.transfer, upload, unit)
				r.ledger.settle(unit, transfer.bytes, upload)
				if ctx.Err() != nil {
					return
				}
				instance.record(quality.ActivitySample{
					ScheduledMS: scheduledMS, StartedMS: startedMS, CompletedMS: r.ms(), FirstByteMS: transfer.firstByteMS,
					Bytes: transfer.bytes, Upload: upload, Failed: transfer.err != nil || transfer.bytes < unit,
				})
			}(upload, unit, scheduledMS)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runProbes issues tiny requests at game cadence on the shared HTTP/1.1
// pool; delay is round-trip time under the household load.
func (r *householdRun) runProbes(ctx context.Context, instance *householdInstance) {
	profile := instance.profile
	endpoint := endpointForProbe(r.downloadURL)
	slots := make(chan struct{}, max(profile.InFlight, 1))
	var wait sync.WaitGroup
	defer wait.Wait()
	interval := time.Duration(profile.IntervalMS) * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	phaseStart := time.Now()
	phaseStartMS := r.ms()
	slot := 0
	for ctx.Err() == nil {
		due := int(time.Since(phaseStart) / interval)
		for ; slot < due; slot++ {
			missedMS := phaseStartMS + int64(slot)*profile.IntervalMS
			instance.record(quality.ActivitySample{ScheduledMS: missedMS, StartedMS: missedMS, CompletedMS: missedMS, Failed: true})
		}
		scheduledMS := phaseStartMS + int64(slot)*profile.IntervalMS
		slot++
		select {
		case slots <- struct{}{}:
			wait.Add(1)
			go func(scheduledMS int64) {
				defer wait.Done()
				defer func() { <-slots }()
				startedMS := r.ms()
				_, ok, exhausted := probeOnce(ctx, r.transfer, endpoint, r.responses)
				if exhausted || ctx.Err() != nil {
					return
				}
				// Delay is measured from the scheduled instant, so queueing
				// behind a busy pool counts against the game, as it would.
				instance.record(quality.ActivitySample{ScheduledMS: scheduledMS, StartedMS: startedMS, CompletedMS: r.ms(), Failed: !ok})
			}(scheduledMS)
		default:
			instance.record(quality.ActivitySample{ScheduledMS: scheduledMS, StartedMS: scheduledMS, CompletedMS: scheduledMS, Failed: true})
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runBursts fetches a page-like burst every interval: one object on a fresh
// connection, then a few warmed objects. Response start is the delivery
// measure; the cold object reports connection setup separately.
func (r *householdRun) runBursts(ctx context.Context, instance *householdInstance) {
	profile := instance.profile
	ticker := time.NewTicker(time.Duration(profile.IntervalMS) * time.Millisecond)
	defer ticker.Stop()
	for ctx.Err() == nil {
		burstMS := r.ms()
		if !r.ledger.claim(profile.ColdBytes) {
			return
		}
		cold := r.transfer1(ctx, r.cold, false, profile.ColdBytes)
		r.ledger.settle(profile.ColdBytes, cold.bytes, false)
		if ctx.Err() != nil {
			return
		}
		instance.record(quality.ActivitySample{ScheduledMS: burstMS, StartedMS: burstMS, CompletedMS: r.ms(), FirstByteMS: cold.firstByteMS, Bytes: cold.bytes, Cold: true, Failed: cold.err != nil})
		for object := 0; object < profile.WarmObjects && ctx.Err() == nil; object++ {
			if !r.ledger.claim(profile.WarmBytes) {
				return
			}
			objectMS := r.ms()
			warm := r.transfer1(ctx, r.transfer, false, profile.WarmBytes)
			r.ledger.settle(profile.WarmBytes, warm.bytes, false)
			if ctx.Err() != nil {
				return
			}
			instance.record(quality.ActivitySample{ScheduledMS: objectMS, StartedMS: objectMS, CompletedMS: r.ms(), FirstByteMS: warm.firstByteMS, Bytes: warm.bytes, Failed: warm.err != nil})
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// transfer1 moves one delivery unit and reports payload bytes and the
// test-relative time of the first response byte.
func (r *householdRun) transfer1(ctx context.Context, client *http.Client, upload bool, amount uint64) householdTransfer {
	requestCtx, cancel := context.WithTimeout(ctx, householdRequestTimeout)
	defer cancel()
	var firstByte atomic.Int64
	requestCtx = httptrace.WithClientTrace(requestCtx, &httptrace.ClientTrace{GotFirstResponseByte: func() { firstByte.Store(r.ms()) }})
	counter := r.down
	if upload {
		counter = r.up
	}
	onBytes := func(int) {}
	if counter != nil {
		onBytes = counter.add
	}
	var request *http.Request
	var body *generatedBody
	var err error
	if upload {
		body = newGeneratedBody(amount, onBytes)
		request, err = http.NewRequestWithContext(requestCtx, http.MethodPost, r.uploadURL, body)
		if err == nil {
			request.ContentLength = int64(amount)
			request.Header.Set("Content-Type", "application/octet-stream")
		}
	} else {
		request, err = http.NewRequestWithContext(requestCtx, http.MethodGet, downloadRequestURL(r.downloadURL, amount), nil)
	}
	if err != nil {
		return householdTransfer{err: err}
	}
	response, err := client.Do(request)
	if err != nil {
		moved := uint64(0)
		if body != nil {
			moved = body.transferred(amount)
		}
		return householdTransfer{bytes: moved, firstByteMS: firstByte.Load(), err: err}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return householdTransfer{firstByteMS: firstByte.Load(), err: errors.New("endpoint returned " + response.Status)}
	}
	if upload {
		if err := drainResponse(response, r.responses); err != nil {
			return householdTransfer{bytes: body.transferred(amount), firstByteMS: firstByte.Load(), err: err}
		}
		return householdTransfer{bytes: body.transferred(amount), firstByteMS: firstByte.Load()}
	}
	var moved uint64
	buffer := make([]byte, transferChunk)
	reader := io.LimitReader(response.Body, int64(amount))
	for {
		count, readErr := reader.Read(buffer)
		if count > 0 {
			moved += uint64(count)
			onBytes(count)
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) && requestCtx.Err() == nil {
				return householdTransfer{bytes: moved, firstByteMS: firstByte.Load(), err: readErr}
			}
			break
		}
	}
	if requestCtx.Err() != nil && ctx.Err() == nil {
		return householdTransfer{bytes: moved, firstByteMS: firstByte.Load(), err: requestCtx.Err()}
	}
	return householdTransfer{bytes: moved, firstByteMS: firstByte.Load()}
}

func sleepUntil(ctx context.Context, at time.Time) bool {
	delay := time.Until(at)
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
