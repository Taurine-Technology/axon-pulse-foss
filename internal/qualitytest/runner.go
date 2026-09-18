// Package qualitytest runs the controller-independent, bounded Internet
// experience test against configurable HTTP endpoints.
package qualitytest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Taurine-Technology/axon-pulse/internal/protocol"
	"github.com/Taurine-Technology/axon-pulse/quality"
)

type (
	Options struct {
		Profile     protocol.SpeedProfileConfig
		Traffic     protocol.TrafficContext
		DownloadURL string
		UploadURL   string
		Now         func() time.Time
		Client      *http.Client
		// OnProgress, when set, receives throttled live snapshots (phase,
		// rates, latest loaded RTT) for local UI streaming. Snapshots are
		// transient and never persisted.
		OnProgress func(protocol.SpeedTestProgress)
		// HouseholdMix and MixSource apply to the household profile only:
		// the activity mix to reproduce and where it came from.
		HouseholdMix quality.HouseholdMix
		MixSource    string
	}

	// progressEmitter serializes live snapshots for Options.OnProgress. A nil
	// emitter is valid and every method is a no-op, so call sites stay
	// unguarded.
	progressEmitter struct {
		mu                                 sync.Mutex
		send                               func(protocol.SpeedTestProgress)
		started                            time.Time
		snapshot                           protocol.SpeedTestProgress
		lastSent                           time.Time
		lastDownloadBytes, lastUploadBytes uint64
		baseDownload, baseUpload           uint64
		lastRateAt                         time.Time
	}

	Runner struct{}

	phaseCounter struct {
		mu      sync.Mutex
		started time.Time
		streams int
		buckets map[int]*quality.Bucket
		bytes   uint64
	}

	loadResult struct {
		result  protocol.ThroughputResult
		buckets []quality.Bucket
	}

	// generatedBody's remaining counter is read for partial-upload reporting while
	// the HTTP transport may still be consuming the body on its own goroutine, so
	// it must be atomic.
	generatedBody struct {
		remaining atomic.Uint64
		onRead    func(int)
	}

	responseBudget struct {
		capacity  uint64
		remaining atomic.Uint64
		used      atomic.Uint64
	}

	pathSamples struct {
		persistent         []float64
		fresh              []float64
		persistentAttempts int
		freshAttempts      int
	}
)

const (
	DefaultDownloadURL = "https://speed.cloudflare.com/__down"
	DefaultUploadURL   = "https://speed.cloudflare.com/__up"
	// Loaded phases on fast links can end in well under a second once the byte
	// budget is spent; probing every 100ms squeezes enough latency samples out
	// of even a short phase to grade it, at negligible response-byte cost.
	probeInterval      = 100 * time.Millisecond
	transferChunk      = 32 << 10
	maxResponseBody    = 4 << 10
	maxResponseReserve = 64 << 10
	// warmupTimeBox bounds calibration transfers by wall clock so slow links
	// never spend seconds of the run window estimating their own rate.
	warmupTimeBox = 1500 * time.Millisecond
)

var (
	// errResponseBudgetExhausted marks a probe stopped by the local response-byte
	// accounting limit; it must never be reported as network packet loss.
	errResponseBudgetExhausted = errors.New("response-byte budget exhausted")
)

func newProgressEmitter(send func(protocol.SpeedTestProgress), id, profile string, started time.Time) *progressEmitter {
	if send == nil {
		return nil
	}
	return &progressEmitter{send: send, started: started, lastRateAt: started, snapshot: protocol.SpeedTestProgress{MeasurementID: id, Profile: profile}}
}

func (p *progressEmitter) phase(name string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.snapshot.Phase = name
	p.snapshot.DownloadMbps, p.snapshot.UploadMbps = 0, 0
	p.baseDownload, p.baseUpload = p.snapshot.DownloadBytes, p.snapshot.UploadBytes
	p.lastDownloadBytes, p.lastUploadBytes = 0, 0
	p.lastRateAt = time.Now()
	p.emitLocked(true)
	p.mu.Unlock()
}

func (p *progressEmitter) baseline(p50 float64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.snapshot.BaselineP50MS = p50
	p.emitLocked(true)
	p.mu.Unlock()
}

func (p *progressEmitter) rtt(value float64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.snapshot.RTTMS = round(value, 3)
	p.emitLocked(false)
	p.mu.Unlock()
}

// rates receives each direction's cumulative in-phase byte count and derives
// an instantaneous rate from the delta since the previous call.
func (p *progressEmitter) rates(downloadBytes, uploadBytes uint64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	now := time.Now()
	elapsed := now.Sub(p.lastRateAt).Seconds()
	if elapsed >= 0.05 {
		p.snapshot.DownloadMbps = round(float64(downloadBytes-p.lastDownloadBytes)*8/elapsed/1_000_000, 2)
		p.snapshot.UploadMbps = round(float64(uploadBytes-p.lastUploadBytes)*8/elapsed/1_000_000, 2)
		p.lastDownloadBytes, p.lastUploadBytes = downloadBytes, uploadBytes
		p.lastRateAt = now
	}
	p.snapshot.DownloadBytes = p.baseDownload + downloadBytes
	p.snapshot.UploadBytes = p.baseUpload + uploadBytes
	p.emitLocked(false)
	p.mu.Unlock()
}

func (p *progressEmitter) done(result protocol.SpeedTest) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.snapshot.Phase = "done"
	p.snapshot.Done = true
	p.snapshot.DownloadMbps = result.Download.ThroughputMbps
	p.snapshot.UploadMbps = result.Upload.ThroughputMbps
	p.emitLocked(true)
	p.mu.Unlock()
}

func (p *progressEmitter) emitLocked(force bool) {
	now := time.Now()
	if !force && now.Sub(p.lastSent) < 150*time.Millisecond {
		return
	}
	p.lastSent = now
	p.snapshot.ElapsedMS = now.Sub(p.started).Milliseconds()
	p.send(p.snapshot)
}

func (r Runner) Run(ctx context.Context, options Options) (protocol.SpeedTest, error) {
	profile := options.Profile
	if profile.Name == protocol.ProfileHousehold {
		return r.runHousehold(ctx, options)
	}
	if !protocol.KnownSpeedProfile(profile.Name) {
		return protocol.SpeedTest{}, errors.New("quality test profile must be household, saturation, content or capacity")
	}
	if profile.MaxDownloadBytes < 512<<10 || profile.MaxUploadBytes < 256<<10 {
		return protocol.SpeedTest{}, errors.New("quality test byte budget is too small")
	}
	if profile.MaxDurationSeconds < 2 || profile.MaxDurationSeconds > 120 {
		return protocol.SpeedTest{}, errors.New("quality test duration is outside local bounds")
	}
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
	// Keep the measurement path on its own connection pool so the latency
	// probes never queue behind the bulk transfer sockets they are observing.
	transferClient := cloneClient(options.Client, false, profile.MaxConcurrentRequests)
	persistentClient := cloneClient(options.Client, false, 1)
	defer transferClient.CloseIdleConnections()
	defer persistentClient.CloseIdleConnections()
	responseBytes := newResponseBudget(min(uint64(maxResponseReserve), profile.MaxDownloadBytes/16))
	bulkDownloadBudget := profile.MaxDownloadBytes - responseBytes.capacity

	result := protocol.SpeedTest{
		MeasurementID: measurementID, Profile: profile.Name, Timestamp: started.Unix(),
		EndpointHost: endpointHost(downloadURL), TrafficContext: options.Traffic,
		ExperienceEligible: profile.CountsTowardExperience,
		Budget: quality.Budget{
			RequestedDownloadBytes: profile.MaxDownloadBytes, RequestedUploadBytes: profile.MaxUploadBytes,
			EffectiveDownloadBytes: profile.MaxDownloadBytes, EffectiveUploadBytes: profile.MaxUploadBytes,
			MaxDurationSeconds: profile.MaxDurationSeconds, MaxConcurrentRequests: profile.MaxConcurrentRequests,
			PredictedBytes: profile.MaxDownloadBytes + profile.MaxUploadBytes,
		},
		Phases: map[string]protocol.PhaseEvidence{},
	}

	progress := newProgressEmitter(options.OnProgress, measurementID, profile.Name, started)
	progress.phase("baseline")
	baselineStart := elapsedMS(started)
	baseline, baselinePaths := collectBaselineLatency(
		runCtx,
		persistentClient,
		downloadURL,
		responseBytes,
		progress.rtt,
	)
	result.Latency = compatibilityLatency(baseline, endpointHost(downloadURL))
	result.Responsiveness.Baseline = baseline
	result.Phases["baseline"] = latencyEvidence(baselineStart, elapsedMS(started), "latency_only_phase")
	progress.baseline(baseline.P50MS)

	// Warm-ups estimate the link rate that sizes every later allocation, so
	// they get enough bytes for a stable estimate but are time-boxed so slow
	// links never spend seconds of their run window on calibration.
	downloadWarmup := min(bulkDownloadBudget/10, uint64(4<<20))
	uploadWarmup := min(profile.MaxUploadBytes/10, uint64(2<<20))
	progress.phase("download_warmup")
	downloadWarmupStart := elapsedMS(started)
	downloadWarmupResult := timeBoxedTransfer(runCtx, transferClient, "download", downloadURL, downloadWarmup, responseBytes, progress)
	downloadWarmupEnd := elapsedMS(started)
	progress.phase("upload_warmup")
	uploadWarmupStart := elapsedMS(started)
	uploadWarmupResult := timeBoxedTransfer(runCtx, transferClient, "upload", uploadURL, uploadWarmup, responseBytes, progress)
	result.Phases["download_warmup"] = warmupEvidence(downloadWarmupResult, downloadWarmupStart, downloadWarmupEnd)
	result.Phases["upload_warmup"] = warmupEvidence(uploadWarmupResult, uploadWarmupStart, elapsedMS(started))

	// Time shares: 30 % per one-way phase and 25 % for the bidirectional
	// phase leave 15 % of the maximum for baseline, warm-ups and cooldown, so
	// the last phase is no longer truncated by the overall deadline whenever
	// the one-way phases run to their caps.
	phaseMaximum := max(2*time.Second, time.Duration(profile.MaxDurationSeconds)*time.Second*3/10)
	phaseMinimum := min(time.Duration(profile.MinDurationSeconds)*time.Second, phaseMaximum)
	bidirectionalWindow := max(2*time.Second, time.Duration(profile.MaxDurationSeconds)*time.Second/4)
	// Allocate bytes from the measured rate so the profile's byte limit acts
	// as a ceiling rather than a target: a phase should end on stability or
	// its time cap, not because a fixed allocation ran dry mid-measurement.
	downloadRate := warmupRate(downloadWarmupResult)
	uploadRate := warmupRate(uploadWarmupResult)
	downloadAvailable := adaptiveAllocation(downloadRate, phaseMaximum+bidirectionalWindow, bulkDownloadBudget-downloadWarmupResult.BytesTransferred)
	uploadAvailable := adaptiveAllocation(uploadRate, phaseMaximum+bidirectionalWindow, profile.MaxUploadBytes-uploadWarmupResult.BytesTransferred)
	downloadStreams, downloadBlock := adaptiveLoad(downloadRate, profile.MaxConcurrentRequests, downloadAvailable)
	uploadStreams, uploadBlock := adaptiveLoad(uploadRate, profile.MaxConcurrentRequests, uploadAvailable)
	// Reserve bytes for the bidirectional phase in proportion to its share of
	// the allocated window, independently per direction, so an asymmetric
	// uplink cannot run dry before the common observation interval ends.
	downloadBidirectionalBudget := bidirectionalReserve(downloadAvailable, phaseMaximum, bidirectionalWindow)
	uploadBidirectionalBudget := bidirectionalReserve(uploadAvailable, phaseMaximum, bidirectionalWindow)
	downloadPrimaryBudget := downloadAvailable - downloadBidirectionalBudget
	uploadPrimaryBudget := uploadAvailable - uploadBidirectionalBudget

	progress.phase("download")
	downloadPrimary, downloadEvidence, downloadLatency, downloadPaths := runSinglePhase(
		runCtx, transferClient, persistentClient, "download", downloadURL, downloadURL, downloadPrimaryBudget,
		downloadStreams, downloadBlock, phaseMinimum, phaseMaximum, started, responseBytes, progress,
	)
	result.Download = downloadPrimary
	result.Phases["download"] = downloadEvidence
	result.Responsiveness.DownloadLoaded = downloadLatency

	progress.phase("upload")
	uploadPrimary, uploadEvidence, uploadLatency, uploadPaths := runSinglePhase(
		runCtx, transferClient, persistentClient, "upload", uploadURL, downloadURL, uploadPrimaryBudget,
		uploadStreams, uploadBlock, phaseMinimum, phaseMaximum, started, responseBytes, progress,
	)
	result.Upload = uploadPrimary
	result.Phases["upload"] = uploadEvidence
	result.Responsiveness.UploadLoaded = uploadLatency

	remainingDownload := min(downloadBidirectionalBudget, bulkDownloadBudget-result.Download.BytesTransferred-downloadWarmupResult.BytesTransferred)
	remainingUpload := min(uploadBidirectionalBudget, profile.MaxUploadBytes-result.Upload.BytesTransferred-uploadWarmupResult.BytesTransferred)
	if remainingDownload >= 128<<10 && remainingUpload >= 128<<10 && runCtx.Err() == nil {
		bidirectionalMaximum := bidirectionalWindow
		bidirectionalMinimum := min(phaseMinimum, bidirectionalMaximum)
		progress.phase("bidirectional")
		bidirectionalDownload, bidirectionalUpload, evidence, latency, paths := runBidirectionalPhase(
			runCtx, transferClient, persistentClient, downloadURL, uploadURL, remainingDownload, remainingUpload,
			min(downloadStreams, 4), min(uploadStreams, 4), downloadBlock, uploadBlock,
			bidirectionalMinimum, bidirectionalMaximum, started, responseBytes, progress,
		)
		result.Responsiveness.BidirectionalDownload = &bidirectionalDownload
		result.Responsiveness.BidirectionalUpload = &bidirectionalUpload
		result.Responsiveness.BidirectionalLoaded = &latency
		result.Phases["bidirectional"] = evidence
		downloadPaths.merge(paths)
	} else {
		result.Phases["bidirectional"] = protocol.PhaseEvidence{StableRegion: quality.StableRegion{Reason: "byte_or_time_budget_exhausted"}}
	}

	progress.phase("cooldown")
	cooldownStart := elapsedMS(started)
	cooldown, cooldownPaths := collectFixedLatency(runCtx, persistentClient, downloadURL, 6, responseBytes, progress.rtt)
	result.Responsiveness.Cooldown = &cooldown
	result.Phases["cooldown"] = latencyEvidence(cooldownStart, elapsedMS(started), "latency_only_phase")

	result.Responsiveness.DownloadBloatP90MS, result.Responsiveness.DownloadGrade = bloat(baseline, downloadLatency)
	result.Responsiveness.UploadBloatP90MS, result.Responsiveness.UploadGrade = bloat(baseline, uploadLatency)
	if result.Responsiveness.BidirectionalLoaded != nil {
		result.Responsiveness.BidirectionalBloatP90MS, result.Responsiveness.BidirectionalGrade = bloat(baseline, *result.Responsiveness.BidirectionalLoaded)
	}
	result.Responsiveness.OverallGrade = worseGrade(result.Responsiveness.DownloadGrade, result.Responsiveness.UploadGrade)
	result.Responsiveness.PrimaryDriver = primaryDriver(result.Responsiveness)
	result.Responsiveness.Profile = "standard"
	result.Responsiveness.TotalDownloadBytes = downloadWarmupResult.BytesTransferred + result.Download.BytesTransferred + throughputBytes(result.Responsiveness.BidirectionalDownload) + responseBytes.used.Load()
	result.Responsiveness.TotalUploadBytes = uploadWarmupResult.BytesTransferred + result.Upload.BytesTransferred + throughputBytes(result.Responsiveness.BidirectionalUpload)
	result.Budget.UsedBytes = result.Responsiveness.TotalDownloadBytes + result.Responsiveness.TotalUploadBytes
	result.Budget.CapHit = result.Budget.UsedBytes >= result.Budget.EffectiveDownloadBytes+result.Budget.EffectiveUploadBytes || runCtx.Err() != nil

	allPaths := baselinePaths
	allPaths.merge(downloadPaths)
	allPaths.merge(uploadPaths)
	allPaths.merge(cooldownPaths)
	result.Paths = allPaths.summary()
	result.Experience = quality.ScoreApplications(quality.ApplicationInput{
		Baseline:        phaseMetrics(&baseline),
		Download:        phaseMetrics(&downloadLatency),
		Upload:          phaseMetrics(&uploadLatency),
		Bidirectional:   phaseMetrics(result.Responsiveness.BidirectionalLoaded),
		DownloadP75Mbps: representativeThroughput(result.Download), UploadP75Mbps: representativeThroughput(result.Upload),
		DownloadAvailable: throughputAvailable(result.Download), UploadAvailable: throughputAvailable(result.Upload),
	})
	result.MeasurementConfidence = quality.GradeConfidence(quality.ConfidenceInput{
		BaselineSamples:   int(baseline.Count),
		PhaseSamples:      map[string]int{"download": int(downloadLatency.Count), "upload": int(uploadLatency.Count), "bidirectional": latencyCount(result.Responsiveness.BidirectionalLoaded)},
		StablePhases:      map[string]bool{"download": downloadEvidence.StableRegion.Stable, "upload": uploadEvidence.StableRegion.Stable, "bidirectional": result.Phases["bidirectional"].StableRegion.Stable},
		ProbeSuccessRatio: allPaths.successRatio(), FreshPathSuccessRatio: allPaths.freshSuccessRatio(),
	})
	if options.Traffic.Contaminated {
		result.MeasurementConfidence.Level = lowerConfidence(result.MeasurementConfidence.Level, "medium")
		result.MeasurementConfidence.Reasons = append(result.MeasurementConfidence.Reasons, "cross_traffic_detected")
	}
	// Name the cause when a phase's byte budget ran out before the minimum
	// load duration: on fast links this profile cannot stabilize, which is a
	// budget property, not a network defect. Consumers can suggest the larger
	// capacity profile instead of implying the connection misbehaved.
	budgetLimited := func(name string, evidence protocol.PhaseEvidence, capped bool) {
		if capped && !evidence.StableRegion.Stable && evidence.EndMS-evidence.StartMS < phaseMinimum.Milliseconds() {
			result.MeasurementConfidence.Reasons = append(result.MeasurementConfidence.Reasons, name+"_budget_limited")
		}
	}
	budgetLimited("download", downloadEvidence, result.Download.CapHit)
	budgetLimited("upload", uploadEvidence, result.Upload.CapHit)
	if result.Responsiveness.BidirectionalDownload != nil && result.Responsiveness.BidirectionalUpload != nil {
		budgetLimited("bidirectional", result.Phases["bidirectional"], result.Responsiveness.BidirectionalDownload.CapHit || result.Responsiveness.BidirectionalUpload.CapHit)
	}
	result.Responsiveness.ConfidenceLevel = result.MeasurementConfidence.Level
	result.Responsiveness.ConfidenceReasons = append([]string(nil), result.MeasurementConfidence.Reasons...)
	result.DurationMS = float64(time.Since(started).Milliseconds())
	progress.done(result)
	if ctx.Err() != nil {
		return result, fmt.Errorf("quality test aborted: %w", ctx.Err())
	}
	if err := requiredEvidenceError(result); err != nil {
		return result, err
	}
	return result, nil
}

func requiredEvidenceError(result protocol.SpeedTest) error {
	switch {
	case result.Download.Error != "":
		return fmt.Errorf("quality test download phase failed: %s", result.Download.Error)
	case result.Upload.Error != "":
		return fmt.Errorf("quality test upload phase failed: %s", result.Upload.Error)
	case !throughputAvailable(result.Download):
		return errors.New("quality test download phase produced no usable throughput")
	case !throughputAvailable(result.Upload):
		return errors.New("quality test upload phase produced no usable throughput")
	case result.Responsiveness.Baseline.Count == 0:
		return errors.New("quality test baseline latency produced no usable samples")
	case result.Responsiveness.DownloadLoaded.Count == 0:
		return errors.New("quality test download latency produced no usable samples")
	case result.Responsiveness.UploadLoaded.Count == 0:
		return errors.New("quality test upload latency produced no usable samples")
	default:
		return nil
	}
}

func newPhaseCounter(streams int) *phaseCounter {
	return &phaseCounter{started: time.Now(), streams: streams, buckets: map[int]*quality.Bucket{}}
}

func (p *phaseCounter) add(bytes int) {
	if bytes <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	index := int(time.Since(p.started) / time.Second)
	bucket := p.buckets[index]
	if bucket == nil {
		bucket = &quality.Bucket{Index: index, StartMS: int64(index) * 1000, StreamCount: p.streams}
		p.buckets[index] = bucket
	}
	bucket.Bytes += uint64(bytes)
	bucket.Samples++
	p.bytes += uint64(bytes)
}

func (p *phaseCounter) snapshot(final bool) []quality.Bucket {
	p.mu.Lock()
	defer p.mu.Unlock()
	elapsed := time.Since(p.started)
	last := int(elapsed / time.Second)
	result := make([]quality.Bucket, 0, len(p.buckets))
	for index, original := range p.buckets {
		bucket := *original
		bucket.DurationMS = 1000
		bucket.Complete = index < last
		if index == last {
			bucket.DurationMS = max(elapsed.Milliseconds()-int64(index)*1000, 1)
			bucket.Complete = final && bucket.DurationMS >= 900
		}
		bucket.ThroughputMbps = float64(bucket.Bytes) * 8 / float64(bucket.DurationMS) / 1000
		result = append(result, bucket)
	}
	slices.SortFunc(result, func(left, right quality.Bucket) int { return left.Index - right.Index })
	return result
}

func (p *phaseCounter) byteCount() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bytes
}

func runSinglePhase(ctx context.Context, transfer, persistent *http.Client, direction, endpoint, probeEndpoint string, budget uint64, streams int, block uint64, minimum, maximum time.Duration, testStarted time.Time, responses *responseBudget, progress *progressEmitter) (protocol.ThroughputResult, protocol.PhaseEvidence, protocol.LatencySummary, pathSamples) {
	phaseCtx, cancel := context.WithTimeout(ctx, maximum)
	defer cancel()
	streams, block = boundedLoad(budget, streams, block)
	counter := newPhaseCounter(streams)
	paths := pathSamples{}
	latencyDone := make(chan protocol.LatencySummary, 1)
	go func() {
		latencyDone <- collectLoadedLatency(phaseCtx, persistent, endpointForProbe(probeEndpoint), &paths, responses, progress.rtt)
	}()
	loadDone := make(chan loadResult, 1)
	go func() {
		loadDone <- executeLoad(phaseCtx, transfer, direction, endpoint, budget, streams, block, counter, responses)
	}()

	startMS := elapsedMS(testStarted)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var loaded loadResult
	for {
		select {
		case loaded = <-loadDone:
			cancel()
			goto finished
		case <-ticker.C:
			if direction == "download" {
				progress.rates(counter.byteCount(), 0)
			} else {
				progress.rates(0, counter.byteCount())
			}
			region := quality.DetectStableRegion(counter.snapshot(false), quality.DefaultStabilityPolicy())
			if time.Since(counter.started) >= minimum && region.Stable {
				cancel()
				loaded = <-loadDone
				goto finished
			}
		case <-phaseCtx.Done():
			loaded = <-loadDone
			goto finished
		}
	}

finished:
	latency := <-latencyDone
	loaded.buckets = counter.snapshot(true)
	loaded.result = summarizeTransfer(loaded.result, loaded.buckets, counter.started, budget, phaseCtx.Err())
	region := quality.DetectStableRegion(loaded.buckets, quality.DefaultStabilityPolicy())
	evidence := protocol.PhaseEvidence{MinDurationMS: minimum.Milliseconds(), MaxDurationMS: maximum.Milliseconds(), StartMS: startMS, EndMS: elapsedMS(testStarted), Buckets: loaded.buckets, StableRegion: region}
	return loaded.result, evidence, latency, paths
}

func runBidirectionalPhase(ctx context.Context, transfer, persistent *http.Client, downloadURL, uploadURL string, downloadBudget, uploadBudget uint64, downloadStreams, uploadStreams int, downloadBlock, uploadBlock uint64, minimum, maximum time.Duration, testStarted time.Time, responses *responseBudget, progress *progressEmitter) (protocol.ThroughputResult, protocol.ThroughputResult, protocol.PhaseEvidence, protocol.LatencySummary, pathSamples) {
	phaseCtx, cancel := context.WithTimeout(ctx, maximum)
	defer cancel()
	downloadStreams, downloadBlock = boundedLoad(downloadBudget, downloadStreams, downloadBlock)
	uploadStreams, uploadBlock = boundedLoad(uploadBudget, uploadStreams, uploadBlock)
	downCounter := newPhaseCounter(downloadStreams)
	upCounter := newPhaseCounter(uploadStreams)
	paths := pathSamples{}
	latencyDone := make(chan protocol.LatencySummary, 1)
	go func() {
		latencyDone <- collectLoadedLatency(phaseCtx, persistent, endpointForProbe(downloadURL), &paths, responses, progress.rtt)
	}()
	downDone := make(chan loadResult, 1)
	upDone := make(chan loadResult, 1)
	go func() {
		downDone <- executeLoad(phaseCtx, transfer, "download", downloadURL, downloadBudget, downloadStreams, downloadBlock, downCounter, responses)
	}()
	go func() {
		upDone <- executeLoad(phaseCtx, transfer, "upload", uploadURL, uploadBudget, uploadStreams, uploadBlock, upCounter, responses)
	}()

	startMS := elapsedMS(testStarted)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	phaseDone := phaseCtx.Done()
	var down, up loadResult
	for downDone != nil || upDone != nil {
		select {
		case down = <-downDone:
			downDone = nil
		case up = <-upDone:
			upDone = nil
		case <-ticker.C:
			progress.rates(downCounter.byteCount(), upCounter.byteCount())
			// Stop early only once each direction is stable on its own over
			// overlapping buckets; a stable sum can hide one direction failing.
			if time.Since(downCounter.started) >= minimum && quality.DetectBidirectionalStableRegion(downCounter.snapshot(false), upCounter.snapshot(false), quality.DefaultStabilityPolicy()).Stable {
				cancel()
			}
		case <-phaseDone:
			// Workers share this context and will drain through their result
			// channels; stop selecting on the always-ready Done case so the
			// loop blocks instead of spinning while transfers wind down.
			phaseDone = nil
		}
	}
	cancel()
	latency := <-latencyDone
	down.buckets, up.buckets = downCounter.snapshot(true), upCounter.snapshot(true)
	down.result = summarizeTransfer(down.result, down.buckets, downCounter.started, downloadBudget, phaseCtx.Err())
	up.result = summarizeTransfer(up.result, up.buckets, upCounter.started, uploadBudget, phaseCtx.Err())
	combined := combineBuckets(down.buckets, up.buckets, downloadStreams+uploadStreams)
	evidence := protocol.PhaseEvidence{MinDurationMS: minimum.Milliseconds(), MaxDurationMS: maximum.Milliseconds(), StartMS: startMS, EndMS: elapsedMS(testStarted), Buckets: combined, StableRegion: quality.DetectBidirectionalStableRegion(down.buckets, up.buckets, quality.DefaultStabilityPolicy())}
	return down.result, up.result, evidence, latency, paths
}

func executeLoad(ctx context.Context, httpClient *http.Client, direction, endpoint string, budget uint64, streams int, block uint64, counter *phaseCounter, responses *responseBudget) loadResult {
	if streams < 1 {
		streams = 1
	}
	if block == 0 {
		block = 512 << 10
	}
	var remaining atomic.Uint64
	remaining.Store(budget)
	var group sync.WaitGroup
	var firstError atomic.Pointer[string]
	for range streams {
		group.Go(func() {
			for ctx.Err() == nil {
				amount := claim(&remaining, block)
				if amount == 0 {
					return
				}
				result := oneTransfer(ctx, httpClient, direction, endpoint, amount, counter.add, responses)
				if result.Error != "" && ctx.Err() == nil {
					errorText := result.Error
					firstError.CompareAndSwap(nil, &errorText)
					return
				}
			}
		})
	}
	group.Wait()
	result := protocol.ThroughputResult{BytesTransferred: counter.byteCount()}
	if errorText := firstError.Load(); errorText != nil {
		result.Error = *errorText
	}
	return loadResult{result: result}
}

// timeBoxedTransfer bounds a calibration transfer by wall clock as well as
// bytes: fast links finish the bytes quickly, slow links yield a partial but
// still rate-usable sample instead of stalling the run window.
func timeBoxedTransfer(ctx context.Context, httpClient *http.Client, direction, endpoint string, amount uint64, responses *responseBudget, progress *progressEmitter) protocol.ThroughputResult {
	boxed, cancel := context.WithTimeout(ctx, warmupTimeBox)
	defer cancel()
	return oneTransfer(boxed, httpClient, direction, endpoint, amount, warmupRateStream(direction, progress), responses)
}

// warmupRateStream feeds calibration transfer bytes into the live progress
// stream so warm-up segments chart real throughput instead of a flat line.
func warmupRateStream(direction string, progress *progressEmitter) func(int) {
	if progress == nil {
		return nil
	}
	var total uint64
	return func(count int) {
		total += uint64(count)
		if direction == "download" {
			progress.rates(total, 0)
		} else {
			progress.rates(0, total)
		}
	}
}

// warmupRate recovers a rate estimate even from a transfer that the time box
// interrupted, where the error path reports bytes and duration but no rate.
func warmupRate(result protocol.ThroughputResult) float64 {
	if result.ThroughputMbps > 0 {
		return result.ThroughputMbps
	}
	if result.BytesTransferred > 0 && result.DurationMS > 0 {
		return float64(result.BytesTransferred) * 8 / (result.DurationMS / 1000) / 1_000_000
	}
	return 0
}

// adaptiveAllocation sizes a direction's byte allocation to sustain the
// measured rate for the full time window with headroom for ramp-up and
// estimation error. With no usable rate estimate it falls back to the
// ceiling, preserving the previous fixed-allocation behavior.
func adaptiveAllocation(rateMbps float64, window time.Duration, ceiling uint64) uint64 {
	if rateMbps <= 0 {
		return ceiling
	}
	need := uint64(rateMbps * 1.5 / 8 * window.Seconds() * 1_000_000)
	return min(ceiling, max(need, uint64(1<<20)))
}

// bidirectionalReserve is the share of a direction's allocation held back
// for the bidirectional phase: its share of the allocated window, never less
// than a quarter, so the phase is funded for its whole window at the rate
// the allocation assumed rather than for a fixed fifth of the bytes.
func bidirectionalReserve(available uint64, phaseMaximum, bidirectionalWindow time.Duration) uint64 {
	total := phaseMaximum + bidirectionalWindow
	if total <= 0 {
		return available / 4
	}
	share := float64(bidirectionalWindow) / float64(total)
	return uint64(float64(available) * max(share, 0.25))
}

func oneTransfer(ctx context.Context, httpClient *http.Client, direction, endpoint string, amount uint64, onBytes func(int), responses *responseBudget) protocol.ThroughputResult {
	if amount == 0 {
		return protocol.ThroughputResult{}
	}
	started := time.Now()
	var request *http.Request
	var uploadBody *generatedBody
	var err error
	if direction == "download" {
		request, err = http.NewRequestWithContext(ctx, http.MethodGet, downloadRequestURL(endpoint, amount), nil)
	} else {
		uploadBody = newGeneratedBody(amount, onBytes)
		request, err = http.NewRequestWithContext(ctx, http.MethodPost, endpoint, uploadBody)
		if err == nil {
			request.ContentLength = int64(amount)
			request.Header.Set("Content-Type", "application/octet-stream")
		}
	}
	if err != nil {
		return protocol.ThroughputResult{Error: err.Error()}
	}
	response, err := httpClient.Do(request)
	if err != nil {
		moved := uint64(0)
		if uploadBody != nil {
			moved = uploadBody.transferred(amount)
		}
		return protocol.ThroughputResult{BytesTransferred: moved, DurationMS: float64(time.Since(started).Milliseconds()), Error: err.Error()}
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return protocol.ThroughputResult{DurationMS: float64(time.Since(started).Milliseconds()), Error: "endpoint returned " + response.Status}
	}
	var moved uint64
	if direction == "download" {
		buffer := make([]byte, transferChunk)
		reader := io.LimitReader(response.Body, int64(amount))
		for {
			count, readErr := reader.Read(buffer)
			if count > 0 {
				moved += uint64(count)
				if onBytes != nil {
					onBytes(count)
				}
			}
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) && ctx.Err() == nil {
					return protocol.ThroughputResult{BytesTransferred: moved, DurationMS: float64(time.Since(started).Milliseconds()), Error: readErr.Error()}
				}
				break
			}
		}
	} else {
		if err := drainResponse(response, responses); err != nil {
			return protocol.ThroughputResult{BytesTransferred: uploadBody.transferred(amount), DurationMS: float64(time.Since(started).Milliseconds()), Error: err.Error()}
		}
		moved = uploadBody.transferred(amount)
	}
	duration := max(time.Since(started), time.Millisecond)
	return protocol.ThroughputResult{BytesTransferred: moved, DurationMS: float64(duration.Milliseconds()), ThroughputMbps: float64(moved) * 8 / duration.Seconds() / 1_000_000}
}

func newGeneratedBody(amount uint64, onRead func(int)) *generatedBody {
	body := &generatedBody{onRead: onRead}
	body.remaining.Store(amount)
	return body
}

func (b *generatedBody) transferred(amount uint64) uint64 {
	return amount - b.remaining.Load()
}

func newResponseBudget(capacity uint64) *responseBudget {
	budget := &responseBudget{capacity: capacity}
	budget.remaining.Store(capacity)
	return budget
}

func drainResponse(response *http.Response, budget *responseBudget) error {
	if budget == nil {
		return errors.New("response-byte budget unavailable")
	}
	reserved := claim(&budget.remaining, uint64(maxResponseBody))
	if reserved == 0 {
		return errResponseBudgetExhausted
	}
	if response.ContentLength > int64(reserved) {
		budget.remaining.Add(reserved)
		return errors.New("endpoint response exceeded bounded allowance")
	}
	limited := io.LimitReader(response.Body, int64(reserved))
	count, err := io.Copy(io.Discard, limited)
	if count > 0 {
		budget.used.Add(uint64(count))
	}
	budget.remaining.Add(reserved - uint64(count))
	if err != nil {
		return err
	}
	if count == int64(reserved) && (response.ContentLength < 0 || response.ContentLength > int64(reserved)) {
		return errors.New("endpoint response exceeded bounded allowance")
	}
	return nil
}

func (b *generatedBody) Read(buffer []byte) (int, error) {
	remaining := b.remaining.Load()
	if remaining == 0 {
		return 0, io.EOF
	}
	count := min(len(buffer), int(remaining))
	clear(buffer[:count])
	b.remaining.Add(^uint64(count - 1))
	if b.onRead != nil {
		b.onRead(count)
	}
	return count, nil
}

func summarizeTransfer(result protocol.ThroughputResult, buckets []quality.Bucket, started time.Time, budget uint64, phaseErr error) protocol.ThroughputResult {
	result.DurationMS = float64(max(time.Since(started).Milliseconds(), 1))
	region := quality.DetectStableRegion(buckets, quality.DefaultStabilityPolicy())
	if region.Stable {
		result.ThroughputMbps = region.ThroughputMbps
		result.P75Mbps = bucketPercentile(buckets, region.StartBucket, region.EndBucket, 75)
	} else if result.DurationMS > 0 {
		result.ThroughputMbps = float64(result.BytesTransferred) * 8 / (result.DurationMS / 1000) / 1_000_000
		result.P75Mbps = bucketPercentile(buckets, -1, -1, 75)
	}
	if result.P75Mbps <= 0 {
		result.P75Mbps = result.ThroughputMbps
	}
	result.CapHit = result.BytesTransferred >= budget || errors.Is(phaseErr, context.DeadlineExceeded)
	if result.Error == "" && phaseErr != nil && !errors.Is(phaseErr, context.Canceled) && !errors.Is(phaseErr, context.DeadlineExceeded) {
		result.Error = phaseErr.Error()
	}
	return result
}

func bucketPercentile(buckets []quality.Bucket, start, end int, pct float64) float64 {
	values := make([]float64, 0, len(buckets))
	for _, bucket := range buckets {
		if !bucket.Complete || bucket.DurationMS < 900 || bucket.Bytes == 0 || (start >= 0 && (bucket.Index < start || bucket.Index > end)) {
			continue
		}
		value := float64(bucket.Bytes) * 8 / float64(bucket.DurationMS) / 1000
		if value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0) {
			values = append(values, value)
		}
	}
	if len(values) == 0 {
		return 0
	}
	slices.Sort(values)
	return round(percentile(values, pct), 3)
}

func representativeThroughput(result protocol.ThroughputResult) float64 {
	if result.P75Mbps > 0 {
		return result.P75Mbps
	}
	return result.ThroughputMbps
}

func (p *pathSamples) addPersistent(value float64, ok bool) {
	p.persistentAttempts++
	if ok {
		p.persistent = append(p.persistent, value)
	}
}

func (p *pathSamples) addFresh(value float64, ok bool) {
	p.freshAttempts++
	if ok {
		p.fresh = append(p.fresh, value)
	}
}

func (p *pathSamples) merge(other pathSamples) {
	p.persistent = append(p.persistent, other.persistent...)
	p.fresh = append(p.fresh, other.fresh...)
	p.persistentAttempts += other.persistentAttempts
	p.freshAttempts += other.freshAttempts
}

func (p *pathSamples) summary() protocol.PathResponsiveness {
	return protocol.PathResponsiveness{Persistent: pathSummary(p.persistent, p.persistentAttempts), Fresh: pathSummary(p.fresh, p.freshAttempts)}
}

func (p *pathSamples) successRatio() float64 {
	attempts := p.persistentAttempts + p.freshAttempts
	if attempts == 0 {
		return 0
	}
	return float64(len(p.persistent)+len(p.fresh)) / float64(attempts)
}

func (p *pathSamples) freshSuccessRatio() float64 {
	if p.freshAttempts == 0 {
		return 0
	}
	return float64(len(p.fresh)) / float64(p.freshAttempts)
}

// collectBaselineLatency keeps the first connection setup in fresh-path
// diagnostics, then measures enough idle requests for a useful p95. Both
// warm-up and sampling obey the test's existing time and response-byte caps.
func collectBaselineLatency(
	ctx context.Context,
	persistent *http.Client,
	endpoint string,
	responses *responseBudget,
	onRTT func(float64),
) (protocol.LatencySummary, pathSamples) {
	paths := pathSamples{}
	if ctx.Err() != nil {
		return latencySummary([]float64{}, 0), paths
	}
	value, ok, exhausted := probeOnce(
		ctx,
		persistent,
		endpointForProbe(endpoint),
		responses,
	)
	if exhausted || (!ok && ctx.Err() != nil) {
		return latencySummary([]float64{}, 0), paths
	}
	paths.addFresh(value, ok)
	baseline, measured := collectFixedLatency(
		ctx,
		persistent,
		endpoint,
		quality.MinimumBaselineSamples,
		responses,
		onRTT,
	)
	paths.merge(measured)
	return baseline, paths
}

func collectFixedLatency(ctx context.Context, persistent *http.Client, endpoint string, count int, responses *responseBudget, onRTT func(float64)) (protocol.LatencySummary, pathSamples) {
	paths := pathSamples{}
	values := make([]float64, 0, count)
	attempts := 0
	// Match loaded-probe cadence instead of adding 100 ms after every
	// response; doubling idle evidence must not double the idle time budget.
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()
	for index := 0; index < count && ctx.Err() == nil; index++ {
		value, ok, budgetExhausted := probeOnce(ctx, persistent, endpointForProbe(endpoint), responses)
		if budgetExhausted {
			break
		}
		if !ok && ctx.Err() != nil {
			break
		}
		attempts++
		paths.addPersistent(value, ok)
		if ok {
			values = append(values, value)
			if onRTT != nil {
				onRTT(value)
			}
		}
		if index%5 == 0 {
			freshValue, freshOK, freshExhausted := freshProbe(ctx, persistent, endpointForProbe(endpoint), responses)
			if !freshExhausted && (freshOK || ctx.Err() == nil) {
				paths.addFresh(freshValue, freshOK)
			}
		}
		if index+1 < count {
			select {
			case <-ctx.Done():
			case <-ticker.C:
			}
		}
	}
	return latencySummary(values, attempts), paths
}

func collectLoadedLatency(ctx context.Context, persistent *http.Client, endpoint string, paths *pathSamples, responses *responseBudget, onRTT func(float64)) protocol.LatencySummary {
	values := []float64{}
	attempts := 0
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		value, ok, budgetExhausted := probeOnce(ctx, persistent, endpoint, responses)
		if budgetExhausted {
			return latencySummary(values, attempts)
		}
		if !ok && ctx.Err() != nil {
			return latencySummary(values, attempts)
		}
		attempts++
		paths.addPersistent(value, ok)
		if ok {
			values = append(values, value)
			if onRTT != nil {
				onRTT(value)
			}
		}
		if attempts%5 == 0 && ctx.Err() == nil {
			freshValue, freshOK, freshExhausted := freshProbe(ctx, persistent, endpoint, responses)
			if !freshExhausted && (freshOK || ctx.Err() == nil) {
				paths.addFresh(freshValue, freshOK)
			}
		}
		select {
		case <-ctx.Done():
			return latencySummary(values, attempts)
		case <-ticker.C:
		}
	}
	return latencySummary(values, attempts)
}

// probeOnce returns the measured RTT and success flag; budgetExhausted reports
// that the shared response-byte budget stopped the probe, which callers must
// treat as end-of-measurement rather than packet loss.
func probeOnce(ctx context.Context, httpClient *http.Client, endpoint string, responses *responseBudget) (value float64, ok, budgetExhausted bool) {
	started := time.Now()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, false, false
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return 0, false, false
	}
	readErr := drainResponse(response, responses)
	_ = response.Body.Close()
	if errors.Is(readErr, errResponseBudgetExhausted) {
		return 0, false, true
	}
	if readErr != nil || response.StatusCode < 200 || response.StatusCode >= 300 {
		return 0, false, false
	}
	rtt := float64(time.Since(started).Microseconds()) / 1000
	rtt = max(0, rtt-serverProcessingMS(response.Header.Values("Server-Timing")))
	return rtt, true, false
}

func freshProbe(ctx context.Context, base *http.Client, endpoint string, responses *responseBudget) (float64, bool, bool) {
	probeClient := freshClient(base)
	value, ok, budgetExhausted := probeOnce(ctx, probeClient, endpoint, responses)
	probeClient.CloseIdleConnections()
	return value, ok, budgetExhausted
}

func freshClient(base *http.Client) *http.Client {
	return cloneClient(base, true, 1)
}

func serverProcessingMS(values []string) float64 {
	for _, value := range values {
		for item := range strings.SplitSeq(value, ",") {
			for parameter := range strings.SplitSeq(item, ";") {
				name, raw, ok := strings.Cut(strings.TrimSpace(parameter), "=")
				if !ok || strings.ToLower(name) != "dur" {
					continue
				}
				duration, err := strconv.ParseFloat(strings.Trim(raw, `"`), 64)
				if err == nil && duration >= 0 && duration < 60_000 {
					return duration
				}
			}
		}
	}
	return 0
}

func latencySummary(values []float64, attempts int) protocol.LatencySummary {
	result := protocol.LatencySummary{Count: uint32(len(values)), Attempts: uint32(attempts), PacketLossValid: attempts > 0}
	if attempts > 0 {
		result.PacketLossPct = 100 * float64(attempts-len(values)) / float64(attempts)
	}
	if len(values) == 0 {
		result.Error = "no latency probes succeeded"
		return result
	}
	ordered := append([]float64(nil), values...)
	slices.Sort(ordered)
	result.MinMS, result.MaxMS = round(ordered[0], 3), round(ordered[len(ordered)-1], 3)
	result.P5MS, result.P50MS = round(percentile(ordered, 5), 3), round(percentile(ordered, 50), 3)
	result.P90MS, result.P95MS, result.P99MS = round(percentile(ordered, 90), 3), round(percentile(ordered, 95), 3), round(percentile(ordered, 99), 3)
	result.JitterIQRMS = round(percentile(ordered, 75)-percentile(ordered, 25), 3)
	var jitter float64
	for index := 1; index < len(values); index++ {
		jitter += math.Abs(values[index] - values[index-1])
	}
	if len(values) > 1 {
		result.JitterMeanDeltaMS = round(jitter/float64(len(values)-1), 3)
	}
	return result
}

func compatibilityLatency(summary protocol.LatencySummary, target string) protocol.LatencyResult {
	return protocol.LatencyResult{MinMS: summary.MinMS, AvgMS: summary.P50MS, MaxMS: summary.MaxMS, JitterMS: summary.JitterMeanDeltaMS, PacketLossPct: summary.PacketLossPct, PacketLossValid: summary.PacketLossValid, ProbesSent: summary.Attempts, Target: target, Error: summary.Error}
}

func pathSummary(values []float64, attempts int) protocol.PathLatency {
	if attempts == 0 {
		return protocol.PathLatency{}
	}
	ordered := append([]float64(nil), values...)
	slices.Sort(ordered)
	result := protocol.PathLatency{Samples: attempts, Successes: len(values), LossPct: 100 * float64(attempts-len(values)) / float64(attempts)}
	if len(ordered) > 0 {
		result.P50MS, result.P95MS = round(percentile(ordered, 50), 3), round(percentile(ordered, 95), 3)
	}
	return result
}

func adaptiveLoad(mbps float64, maximum int, budget uint64) (int, uint64) {
	maximum = min(max(maximum, 1), 8)
	switch {
	case mbps < 2:
		return min(maximum, 2), min(budget, uint64(512<<10))
	case mbps <= 400:
		return min(maximum, 6), min(budget, uint64(4<<20))
	default:
		return min(maximum, 8), min(budget, uint64(8<<20))
	}
}

func boundedLoad(budget uint64, streams int, block uint64) (int, uint64) {
	streams = min(max(streams, 1), 8)
	if budget == 0 {
		return 1, 0
	}
	// Give every advertised stream an initial allocation. Without this clamp a
	// single worker can claim the whole bounded content profile while evidence
	// incorrectly reports four or eight concurrent requests.
	minimumPerStream := uint64(64 << 10)
	streams = min(streams, max(int(budget/minimumPerStream), 1))
	perStream := max(budget/uint64(streams), 1)
	if block == 0 {
		block = min(perStream, uint64(512<<10))
	}
	return streams, min(block, perStream)
}

func combineBuckets(left, right []quality.Bucket, streams int) []quality.Bucket {
	combined := map[int]quality.Bucket{}
	counts := map[int]int{}
	for _, source := range [][]quality.Bucket{left, right} {
		for _, bucket := range source {
			value := combined[bucket.Index]
			if counts[bucket.Index] == 0 {
				value.Complete = true
			}
			value.Index, value.StartMS, value.StreamCount = bucket.Index, bucket.StartMS, streams
			value.Bytes += bucket.Bytes
			value.Samples += bucket.Samples
			value.DurationMS = max(value.DurationMS, bucket.DurationMS)
			value.Complete = value.Complete && bucket.Complete
			combined[bucket.Index] = value
			counts[bucket.Index]++
		}
	}
	result := make([]quality.Bucket, 0, len(combined))
	for _, bucket := range combined {
		bucket.Complete = bucket.Complete && counts[bucket.Index] == 2
		if bucket.DurationMS > 0 {
			bucket.ThroughputMbps = float64(bucket.Bytes) * 8 / float64(bucket.DurationMS) / 1000
		}
		result = append(result, bucket)
	}
	slices.SortFunc(result, func(a, b quality.Bucket) int { return a.Index - b.Index })
	return result
}

func claim(remaining *atomic.Uint64, maximum uint64) uint64 {
	for {
		available := remaining.Load()
		if available == 0 {
			return 0
		}
		amount := min(available, maximum)
		if remaining.CompareAndSwap(available, available-amount) {
			return amount
		}
	}
}

func client(fresh bool, concurrency int) *http.Client {
	transport := &http.Transport{
		DialContext: protocol.DialPublicContext, ForceAttemptHTTP2: true,
		DisableKeepAlives: fresh, MaxIdleConns: max(concurrency*2, 2), MaxIdleConnsPerHost: max(concurrency, 1),
		MaxConnsPerHost: max(concurrency, 1),
		IdleConnTimeout: 30 * time.Second, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second,
	}
	return &http.Client{Transport: transport, CheckRedirect: rejectRedirect}
}

func cloneClient(base *http.Client, fresh bool, concurrency int) *http.Client {
	if base == nil {
		return client(fresh, concurrency)
	}
	baseTransport := base.Transport
	if baseTransport == nil {
		baseTransport = http.DefaultTransport
	}
	transport, ok := baseTransport.(*http.Transport)
	if !ok {
		// Custom transports are primarily a test/integration hook and cannot be
		// cloned safely. Preserve their behavior instead of guessing at it.
		return &http.Client{Transport: baseTransport, Timeout: base.Timeout, CheckRedirect: rejectRedirect, Jar: base.Jar}
	}
	cloned := transport.Clone()
	cloned.DisableKeepAlives = fresh
	cloned.MaxIdleConns = max(concurrency*2, 2)
	cloned.MaxIdleConnsPerHost = max(concurrency, 1)
	cloned.MaxConnsPerHost = max(concurrency, 1)
	if fresh {
		cloned.MaxIdleConns, cloned.MaxIdleConnsPerHost = 0, 0
	}
	return &http.Client{
		Transport: cloned, Timeout: base.Timeout, CheckRedirect: rejectRedirect,
		Jar: base.Jar,
	}
}

func warmupEvidence(result protocol.ThroughputResult, startMS, endMS int64) protocol.PhaseEvidence {
	duration := max(endMS-startMS, int64(result.DurationMS))
	return protocol.PhaseEvidence{StartMS: startMS, EndMS: endMS, Buckets: []quality.Bucket{{Index: 0, StartMS: startMS, DurationMS: duration, Bytes: result.BytesTransferred, Samples: 1, StreamCount: 1, Complete: duration >= 900}}, StableRegion: quality.StableRegion{Reason: "calibration_excluded"}}
}

func latencyEvidence(startMS, endMS int64, reason string) protocol.PhaseEvidence {
	return protocol.PhaseEvidence{StartMS: startMS, EndMS: endMS, StableRegion: quality.StableRegion{Reason: reason}}
}

func bloat(baseline, loaded protocol.LatencySummary) (float64, string) {
	if baseline.Count < 3 || loaded.Count < 3 {
		return 0, ""
	}
	delta := max(0, loaded.P90MS-baseline.P5MS)
	if math.Abs(delta) < 2 {
		delta = 0
	}
	return round(delta, 3), bufferbloatGrade(delta)
}

func bufferbloatGrade(value float64) string {
	switch {
	case value < 5:
		return "A+"
	case value < 30:
		return "A"
	case value < 60:
		return "B"
	case value < 200:
		return "C"
	case value < 400:
		return "D"
	default:
		return "F"
	}
}

func worseGrade(values ...string) string {
	ranks := map[string]int{"A+": 0, "A": 1, "B": 2, "C": 3, "D": 4, "F": 5}
	worst, worstRank := "", -1
	for _, value := range values {
		if rank, ok := ranks[value]; ok && rank > worstRank {
			worst, worstRank = value, rank
		}
	}
	return worst
}

func primaryDriver(result protocol.Responsiveness) string {
	if result.UploadGrade != "" && result.UploadBloatP90MS >= result.DownloadBloatP90MS && result.UploadBloatP90MS >= 5 {
		return "upload"
	}
	if result.DownloadGrade != "" && result.DownloadBloatP90MS >= 5 {
		return "download"
	}
	return ""
}

func phaseMetrics(summary *protocol.LatencySummary) quality.PhaseMetrics {
	if summary == nil {
		return quality.PhaseMetrics{}
	}
	return quality.PhaseMetrics{P95LatencyMS: summary.P95MS, LossPct: summary.PacketLossPct, LatencySamples: int(summary.Count), LossAvailable: summary.PacketLossValid}
}

func throughputAvailable(result protocol.ThroughputResult) bool {
	return result.BytesTransferred > 0 && representativeThroughput(result) > 0
}

func rejectRedirect(_ *http.Request, _ []*http.Request) error {
	return errors.New("quality-test endpoint redirect rejected")
}

func latencyCount(summary *protocol.LatencySummary) int {
	if summary == nil {
		return 0
	}
	return int(summary.Count)
}

func throughputBytes(result *protocol.ThroughputResult) uint64 {
	if result == nil {
		return 0
	}
	return result.BytesTransferred
}

func percentile(sorted []float64, pct float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	position := pct / 100 * float64(len(sorted)-1)
	lower, upper := int(math.Floor(position)), int(math.Ceil(position))
	if lower == upper {
		return sorted[lower]
	}
	return sorted[lower] + (position-float64(lower))*(sorted[upper]-sorted[lower])
}

func downloadRequestURL(raw string, bytes uint64) string {
	parsed, _ := url.Parse(raw)
	query := parsed.Query()
	query.Set("bytes", strconv.FormatUint(bytes, 10))
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func endpointForProbe(raw string) string { return downloadRequestURL(raw, 0) }

func validateEndpoint(raw string, allowPrivate bool) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
		return errors.New("endpoint must be an HTTPS URL without user information")
	}
	if !allowPrivate && ((parsed.Port() != "" && parsed.Port() != "443") || !protocol.IsSafePublicHost(parsed.Hostname())) {
		return errors.New("endpoint must be a public HTTPS host on port 443")
	}
	return nil
}

func endpointHost(raw string) string {
	parsed, _ := url.Parse(raw)
	return parsed.Hostname()
}

func measurementID(started time.Time) string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err == nil {
		return "m-" + hex.EncodeToString(value[:])
	}
	return "m-" + strconv.FormatInt(started.UnixNano(), 36)
}

func elapsedMS(started time.Time) int64 { return time.Since(started).Milliseconds() }
func now(clock func() time.Time) time.Time {
	if clock != nil {
		return clock()
	}
	return time.Now()
}
func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
func lowerConfidence(left, right string) string {
	rank := map[string]int{"high": 0, "medium": 1, "low": 2}
	if rank[right] > rank[left] {
		return right
	}
	return left
}
func round(value float64, places int) float64 {
	factor := math.Pow10(places)
	return math.Round(value*factor) / factor
}
