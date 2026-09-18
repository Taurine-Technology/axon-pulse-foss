package diagnostics

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/Taurine-Technology/axon-contracts/gen/go/buildinfo"
)

const (
	defaultDownloadURL      = "https://speed.cloudflare.com/__down"
	defaultUploadURL        = "https://speed.cloudflare.com/__up"
	defaultPingTarget       = "1.1.1.1"
	defaultMaxDurationSec   = 20
	defaultMaxDownloadBytes = 15 * 1024 * 1024
	defaultMaxUploadBytes   = 8 * 1024 * 1024
	defaultPingCount        = 20
	maxPingCount            = 100
	loadedPingCount         = 5

	httpChunk        = 65536
	warmupBytes      = 100_000
	minMeasuredStage = 128 * 1024

	// Download streaming-measurement window (see measureDownloadStream).
	// Cloudflare's __down caps a single request at < 100 MB; 50 MB is the
	// largest safe value, re-requested back-to-back for a continuous stream.
	//
	// Data budget (à la Google/M-Lab: "usually < 40 MB, more on fast
	// links"): a short warmup discards TCP slow-start, then the tail is
	// measured until two slices converge. On a ~300 Mbps line a typical
	// run early-stops around 1 s of transfer (~40 MB); slower links use
	// proportionally less, faster links a bit more (capped by the ceiling).
	maxDownloadRequestBytes = 50_000_000
	downloadWarmupDuration  = 400 * time.Millisecond // TCP slow-start only
	downloadSliceDuration   = 250 * time.Millisecond // steady-state sampling granularity
	downloadMinMeasure      = 600 * time.Millisecond
	downloadMaxMeasure      = 2 * time.Second
	downloadStableThreshold = 0.08              // consecutive slices within 8% → converged
	defaultDownloadCeiling  = 150 * 1000 * 1000 // safety cap when nothing else stops it

	speedEndpointTimeout = 10 * time.Second
	pingInterval         = 200 * time.Millisecond
)

var (
	// Download uses continuous wall-clock streaming (measureDownloadStream),
	// not a chunked ramp. Upload still ramps in growing chunks — a POST is
	// timed by whole-request wall clock, which the HTTP/2 read-ahead
	// artifact does not affect.
	uploadStages = []uint64{256 * 1024, 1 * 1024 * 1024, 4 * 1024 * 1024}

	pingRTTRE     = regexp.MustCompile(`time[=<]([\d.]+)\s*ms`)
	pingLossRE    = regexp.MustCompile(`([\d.]+)%\s*packet\s*loss`)
	pingTxRE      = regexp.MustCompile(`(\d+)\s*packets\s*transmitted`)
	uploadPayload = newUploadPayload()

	errUploadDeadline = errors.New("upload deadline reached")
)

type (
	// SpeedEndpoint is the persistent endpoint surface used by RunSpeedTest.
	SpeedEndpoint interface {
		Host() string
		Download(context.Context, uint64, time.Time) (uint64, time.Duration, time.Duration, error)
		// StreamDownload issues one request for nbytes and invokes onRead(n)
		// for each chunk read, letting the caller sample cumulative progress
		// by WALL CLOCK. It returns when the body is exhausted, the deadline
		// trips, or onRead returns false (caller-requested stop).
		StreamDownload(ctx context.Context, nbytes uint64, deadline time.Time, onRead func(int) bool) error
		Upload(context.Context, uint64, time.Time) (uint64, time.Duration, error)
		Close() error
	}
	// CPUSample is an operating-system aggregate busy/total CPU counter pair.
	CPUSample struct {
		Busy  uint64
		Total uint64
	}
	// ThroughputResult is one direction's capped ramp result.
	ThroughputResult struct {
		BytesTransferred uint64
		DurationMS       uint32
		ThroughputMbps   float64
		CapHit           bool
		TTFBMS           uint32
		Error            string
	}
	// LatencyResult is the ICMP latency/jitter/loss result.
	LatencyResult struct {
		MinMS           float64
		AvgMS           float64
		MaxMS           float64
		JitterMS        float64
		PacketLossPct   float64
		PacketLossValid bool
		ProbesSent      uint32
		LoadedAvgMS     float64
		Target          string
		Error           string
		// SamplesMS is bounded by maxPingCount and is used only to build phase
		// summaries. Raw samples are not emitted on the wire.
		SamplesMS []float64
	}
	// SpeedTestResult is the proto-agnostic capped speed-test result.
	SpeedTestResult struct {
		Download       ThroughputResult
		Upload         ThroughputResult
		Latency        LatencyResult
		EndpointHost   string
		CPUBusyPct     float64
		DurationMS     uint32
		Responsiveness ResponsivenessResult
	}
	// SpeedTestOptions configures RunSpeedTest.
	SpeedTestOptions struct {
		DownloadURL                   string
		UploadURL                     string
		PingTarget                    string
		MaxDurationSeconds            uint32
		MaxDownloadBytes              uint64
		MaxUploadBytes                uint64
		PingCount                     uint32
		SkipLoadedLatency             bool
		Profile                       SpeedTestProfile
		IncludeBidirectionalInOverall bool

		EndpointFactory func(string) (SpeedEndpoint, error)
		Ping            func(context.Context, string, uint32) LatencyResult
		CPUReader       func() (CPUSample, bool)
	}
	transferFunc func(context.Context, uint64, time.Time) (uint64, time.Duration, time.Duration, error)
	httpEndpoint struct {
		u         *url.URL
		host      string
		client    *http.Client
		transport *http.Transport
	}
	uploadBody struct {
		remaining uint64
		sent      uint64
		deadline  time.Time
	}
)

// RunSpeedTest runs the capped speed test. It deliberately stops on byte
// budget, time budget, or stable ramp samples rather than trying to saturate the
// link.
func RunSpeedTest(ctx context.Context, opts SpeedTestOptions) SpeedTestResult {
	if ctx == nil {
		ctx = context.Background()
	}
	started := time.Now()
	downloadURL := defaultString(opts.DownloadURL, defaultDownloadURL)
	uploadURL := defaultString(opts.UploadURL, defaultUploadURL)
	pingTarget := defaultString(opts.PingTarget, defaultPingTarget)
	maxDuration := time.Duration(defaultMaxDurationSec) * time.Second
	if opts.MaxDurationSeconds != 0 {
		maxDuration = time.Duration(opts.MaxDurationSeconds) * time.Second
	}
	downloadBudget := uint64(defaultMaxDownloadBytes)
	if opts.MaxDownloadBytes != 0 {
		downloadBudget = opts.MaxDownloadBytes
	}
	uploadBudget := uint64(defaultMaxUploadBytes)
	if opts.MaxUploadBytes != 0 {
		uploadBudget = opts.MaxUploadBytes
	}
	pingCount := opts.PingCount
	if pingCount == 0 {
		pingCount = defaultPingCount
	}
	pingCount = clampPingCount(pingCount)
	profile := normalizeSpeedTestProfile(opts.Profile)
	endpointFactory := opts.EndpointFactory
	if endpointFactory == nil {
		endpointFactory = newHTTPEndpoint
	}
	pingFn := opts.Ping
	if pingFn == nil {
		pingFn = RunPing
	}
	cpuReader := opts.CPUReader
	if cpuReader == nil {
		cpuReader = ReadCPUBusy
	}

	overallDeadline := started.Add(maxDuration)
	// Every phase — including all ping runs — must stop at the overall
	// deadline, so RunPing's own timeout is clamped via this context.
	runCtx, cancelRun := context.WithDeadline(ctx, overallDeadline)
	defer cancelRun()
	cpuBefore, haveCPUBefore := cpuReader()

	latency := pingFn(runCtx, pingTarget, pingCount)
	latency.Target = pingTarget

	downloadBudgetForPhase, bidirectionalDownloadBudget := splitResponsivenessBudget(downloadBudget, profile)
	uploadBudgetForPhase, bidirectionalUploadBudget := splitResponsivenessBudget(uploadBudget, profile)

	download := ThroughputResult{Error: "download endpoint not run"}
	upload := ThroughputResult{Error: "upload endpoint not run"}
	var downloadLoaded LatencyResult
	var uploadLoaded LatencyResult
	var bidirectionalLoaded LatencyResult
	var cooldown LatencyResult
	var bidirectionalDownload ThroughputResult
	var bidirectionalUpload ThroughputResult
	endpointHost := ""
	loadedProbeCount := uint32(loadedPingCount)
	if profile != SpeedTestProfileQuick {
		loadedProbeCount = min(pingCount, uint32(minimumReliablePercentileSamples))
	}

	downloadDeadline := proportionalDeadline(overallDeadline, 0.55)
	if profile != SpeedTestProfileQuick {
		downloadDeadline = proportionalDeadline(overallDeadline, 0.35)
	}
	if !time.Now().Before(overallDeadline) {
		download = ThroughputResult{
			CapHit: true,
			Error:  "overall time budget exhausted before download phase",
		}
	} else if endpoint, err := endpointFactory(downloadURL); err != nil {
		download = ThroughputResult{Error: err.Error()}
	} else {
		endpointHost = endpoint.Host()
		func() {
			defer func() { _ = endpoint.Close() }()
			if opts.SkipLoadedLatency || latency.Error != "" {
				download = measureDownloadStream(
					runCtx,
					endpoint,
					downloadDeadline,
					downloadBudgetForPhase,
				)
				return
			}
			download, downloadLoaded = measureThroughputWithLatency(
				runCtx,
				phaseLatencySpec{
					deadline: downloadDeadline,
					target:   pingTarget,
					count:    loadedProbeCount,
					ping:     pingFn,
				},
				func(phaseCtx context.Context) ThroughputResult {
					return measureDownloadStream(
						phaseCtx,
						endpoint,
						downloadDeadline,
						downloadBudgetForPhase,
					)
				},
			)
		}()
	}
	if downloadLoaded.AvgMS != 0 {
		latency.LoadedAvgMS = roundTo(downloadLoaded.AvgMS, 3)
	}

	if !time.Now().Before(overallDeadline) {
		upload = ThroughputResult{
			CapHit: true,
			Error:  "overall time budget exhausted before upload phase",
		}
	} else {
		upEndpoint, err := endpointFactory(uploadURL)
		if err != nil {
			upload = ThroughputResult{Error: err.Error()}
		} else {
			if endpointHost == "" {
				endpointHost = upEndpoint.Host()
			}
			func() {
				defer func() { _ = upEndpoint.Close() }()
				uploadDeadline := overallDeadline
				if profile != SpeedTestProfileQuick {
					uploadDeadline = proportionalDeadline(overallDeadline, 0.45)
				}
				runUpload := func(phaseCtx context.Context) ThroughputResult {
					return runUploadPhase(
						phaseCtx,
						upEndpoint,
						uploadBudgetForPhase,
						uploadDeadline,
					)
				}
				if opts.SkipLoadedLatency || latency.Error != "" {
					upload = runUpload(runCtx)
					return
				}
				upload, uploadLoaded = measureThroughputWithLatency(
					runCtx,
					phaseLatencySpec{
						deadline: uploadDeadline,
						target:   pingTarget,
						count:    loadedProbeCount,
						ping:     pingFn,
					},
					runUpload,
				)
			}()
		}
	}

	if profile != SpeedTestProfileQuick {
		if time.Now().Before(overallDeadline) {
			bidirectionalDeadline := proportionalDeadline(overallDeadline, 0.65)
			bidirectionalDownload, bidirectionalUpload, bidirectionalLoaded = runBidirectionalPhase(
				runCtx,
				bidirectionalPhaseSpec{
					downloadURL:    downloadURL,
					uploadURL:      uploadURL,
					downloadBudget: bidirectionalDownloadBudget,
					uploadBudget:   bidirectionalUploadBudget,
					deadline:       bidirectionalDeadline,
					latency: phaseLatencySpec{
						deadline: bidirectionalDeadline,
						target:   pingTarget,
						count:    loadedProbeCount,
						ping:     pingFn,
					},
					endpointFactory: endpointFactory,
					skipLatency:     opts.SkipLoadedLatency || latency.Error != "",
				},
			)
			if endpointHost == "" {
				endpointHost = defaultString(
					bidirectionalEndpointHost(downloadURL, uploadURL),
					endpointHost,
				)
			}
		} else {
			errText := "overall time budget exhausted before bidirectional phase"
			bidirectionalDownload = ThroughputResult{CapHit: true, Error: errText}
			bidirectionalUpload = ThroughputResult{CapHit: true, Error: errText}
			bidirectionalLoaded = LatencyResult{Error: errText}
		}
	}

	if profile != SpeedTestProfileQuick {
		if time.Now().Before(overallDeadline) {
			cooldown = pingFn(runCtx, pingTarget, loadedProbeCount)
			cooldown.Target = pingTarget
		} else {
			cooldown = LatencyResult{Error: "overall time budget exhausted before cooldown phase"}
		}
	}

	cpuAfter, haveCPUAfter := cpuReader()
	cpuPct := 0.0
	if haveCPUBefore && haveCPUAfter {
		cpuPct = cpuBusyPct(cpuBefore, cpuAfter)
	}
	responsiveness := buildResponsiveness(responsivenessInput{
		profile:              profile,
		includeBidirectional: opts.IncludeBidirectionalInOverall,
		baseline:             latency,
		downloadLoaded:       downloadLoaded,
		uploadLoaded:         uploadLoaded,
		bidirectionalLoaded:  bidirectionalLoaded,
		cooldown:             cooldown,
		transfers: responsivenessTransfers{
			download:              download,
			upload:                upload,
			bidirectionalDownload: bidirectionalDownload,
			bidirectionalUpload:   bidirectionalUpload,
		},
		cpuBusyPct: cpuPct,
	})

	return SpeedTestResult{
		Download:       download,
		Upload:         upload,
		Latency:        latency,
		EndpointHost:   endpointHost,
		CPUBusyPct:     cpuPct,
		DurationMS:     durationMS(time.Since(started)),
		Responsiveness: responsiveness,
	}
}

// runRamp transfers in growing chunks until the byte budget is met or the
// deadline trips, then reports the STEADY-STATE tail rate.
//
// Two deliberate design points, learned the hard way (a 300 Mbps line
// reported 620):
//
//  1. It does NOT stop early on "two consecutive samples look stable".
//     Many ISPs let a connection burst at the physical line rate for the
//     first tens of MB before a token-bucket shaper clamps it to the
//     provisioned rate; two early burst samples look perfectly stable at
//     600 Mbps, so a converge-and-stop ramp would lock onto the burst and
//     never see the shaped rate. We keep pulling until the byte budget
//     (sized to outlast the burst bucket) or the time deadline.
//  2. It reports the average of the tail HALF of the samples, not the
//     peak and not the whole-transfer average — the tail drops the burst
//     ramp-up and converges to the sustained shaped rate.
//
// CapHit now means "truncated by the TIME budget before the full byte
// sample" (a genuine floor: a slow link that may not have finished
// ramping). Reaching the byte budget is a complete, accurate sample, so
// CapHit stays false there.
func runRamp(ctx context.Context, transfer transferFunc, stages []uint64, byteBudget uint64, deadline time.Time) ThroughputResult {
	samples := make([]float64, 0)
	var totalBytes uint64
	var totalTransfer time.Duration
	var firstTTFB time.Duration
	var truncated bool
	var errText string

	stageIdx := 0
	// Loop until the byte budget is met (a full, accurate sample); the
	// deadline/short-read cases below set truncated and break early.
	for totalBytes < byteBudget {
		if !time.Now().Before(deadline) {
			truncated = true
			break
		}
		stage := stages[min(stageIdx, len(stages)-1)]
		remaining := byteBudget - totalBytes
		if stage > remaining {
			stage = remaining
		}
		if stage < minMeasuredStage && len(samples) > 0 {
			// Within a sub-measurable chunk of the budget; the sample is
			// as complete as it will get. Not a truncation.
			break
		}

		moved, ttfb, transferDuration, err := transfer(ctx, stage, deadline)
		if err != nil {
			// A deadline/cancellation error can still carry a real partial
			// measurement; keep its bytes rather than under-reporting the
			// traffic the phase actually moved.
			if moved > 0 {
				moved = min(moved, stage)
				if len(samples) == 0 {
					firstTTFB = ttfb
				}
				if transferDuration <= 0 {
					transferDuration = time.Microsecond
				}
				totalBytes += moved
				totalTransfer += transferDuration
				samples = append(samples, float64(moved)*8/transferDuration.Seconds()/1e6)
				truncated = true
			}
			errText = err.Error()
			if errText == "" {
				errText = fmt.Sprintf("%T", err)
			}
			break
		}
		if moved == 0 {
			errText = "transfer moved no data"
			break
		}
		moved = min(moved, stage)
		if len(samples) == 0 {
			firstTTFB = ttfb
		}
		if transferDuration <= 0 {
			transferDuration = time.Microsecond
		}
		totalBytes += moved
		totalTransfer += transferDuration
		samples = append(samples, float64(moved)*8/transferDuration.Seconds()/1e6)

		if moved < stage {
			// Short read — the deadline clipped this transfer.
			truncated = true
			break
		}
		stageIdx++
	}

	result := ThroughputResult{
		BytesTransferred: totalBytes,
		DurationMS:       durationMS(totalTransfer),
		CapHit:           truncated,
		TTFBMS:           durationMS(firstTTFB),
	}
	if len(samples) == 0 {
		result.Error = defaultString(errText, "no measurement completed")
		return result
	}
	result.ThroughputMbps = roundTo(sustainedThroughputMbps(samples), 3)
	if errText != "" {
		result.Error = "partial: " + errText
	}
	return result
}

// measureDownloadStream measures download throughput the way curl / NDT
// do — a single continuous stream sampled by WALL CLOCK — instead of
// summing per-request body-read times. The old per-chunk approach, over
// Go's HTTP/2 transport, undercounted transfer time and read the ISP's
// burst allowance rather than the shaped rate (a 300 Mbps line reported
// ~500; curl over the same connection reads 300).
//
// The first downloadWarmupDuration is discarded (TCP ramp-up + any ISP
// token-bucket burst), then the sustained rate is measured over the tail
// until consecutive slices converge (early-stop — data-efficient), a max
// window, the byte ceiling, or the deadline. CapHit means the deadline
// truncated the measurement before a full window (a slow-link floor).
func measureDownloadStream(ctx context.Context, endpoint SpeedEndpoint, deadline time.Time, byteCeiling uint64) ThroughputResult {
	if byteCeiling == 0 {
		byteCeiling = defaultDownloadCeiling
	}
	start := time.Now()
	var cum, warmBytes uint64
	var firstByteAt, warmAt, sliceStart time.Time
	var sliceBytes uint64
	var sliceRates []float64
	warmed := false
	truncated := false

	onRead := func(n int) bool {
		now := time.Now()
		if cum == 0 {
			firstByteAt = now
		}
		cum += uint64(n)
		sliceBytes += uint64(n)

		if !warmed {
			if now.Sub(start) >= downloadWarmupDuration {
				warmed = true
				warmBytes = cum
				warmAt = now
				sliceStart = now
				sliceBytes = 0
			}
		} else if now.Sub(sliceStart) >= downloadSliceDuration {
			rate := float64(sliceBytes) * 8 / now.Sub(sliceStart).Seconds() / 1e6
			sliceRates = append(sliceRates, rate)
			sliceStart = now
			sliceBytes = 0
			if now.Sub(warmAt) >= downloadMinMeasure && len(sliceRates) >= 2 {
				a, b := sliceRates[len(sliceRates)-2], sliceRates[len(sliceRates)-1]
				if max(a, b) > 0 && math.Abs(a-b)/max(a, b) <= downloadStableThreshold {
					return false
				}
			}
			if now.Sub(warmAt) >= downloadMaxMeasure {
				return false
			}
		}
		if cum >= byteCeiling {
			return false
		}
		if !now.Before(deadline) {
			truncated = true
			return false
		}
		return true
	}

	var lastErr string
	for time.Now().Before(deadline) {
		stopEarly := false
		wrapped := func(n int) bool {
			ok := onRead(n)
			if !ok {
				stopEarly = true
			}
			return ok
		}
		if cum >= byteCeiling {
			break
		}
		requestBytes := min(uint64(maxDownloadRequestBytes), byteCeiling-cum)
		err := endpoint.StreamDownload(ctx, requestBytes, deadline, wrapped)
		if err != nil {
			if cum == 0 {
				return ThroughputResult{Error: err.Error()}
			}
			lastErr = err.Error()
			break
		}
		if stopEarly {
			break
		}
		// Otherwise the response was exhausted (EOF) before the window
		// closed — re-request on the keep-alive connection to keep the
		// stream continuous.
	}

	if !warmed {
		// Deadline/EOF before the warmup window elapsed (slow link or a
		// short overall budget): report the whole transfer as a floor.
		warmBytes = 0
		warmAt = start
		truncated = true
	}
	measBytes := cum - warmBytes
	measTime := time.Since(warmAt)
	if measTime <= 0 {
		measTime = time.Microsecond
	}
	ttfb := time.Duration(0)
	if !firstByteAt.IsZero() {
		ttfb = firstByteAt.Sub(start)
	}
	result := ThroughputResult{
		BytesTransferred: cum,
		DurationMS:       durationMS(measTime),
		ThroughputMbps:   roundTo(float64(measBytes)*8/measTime.Seconds()/1e6, 3),
		CapHit:           truncated,
		TTFBMS:           durationMS(ttfb),
	}
	if lastErr != "" {
		result.Error = "partial: " + lastErr
	}
	return result
}

// sustainedThroughputMbps returns the steady-state rate: the average of
// the tail half of the ramp samples. Since the stages grow, the first
// half is the ramp-up (and any ISP burst that rides above the shaped
// rate); the tail is the sustained rate. For a single sample there is no
// ramp to drop, so it is returned as-is.
func sustainedThroughputMbps(samples []float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	tail := samples[len(samples)/2:]
	return averageFloat64(tail)
}

func parsePingOutput(output string) ([]float64, float64, int, bool) {
	matches := pingRTTRE.FindAllStringSubmatch(output, -1)
	rtts := make([]float64, 0, len(matches))
	for _, match := range matches {
		if v, err := strconv.ParseFloat(match[1], 64); err == nil {
			rtts = append(rtts, v)
		}
	}
	lossPct := 100.0
	haveLossSummary := false
	if match := pingLossRE.FindStringSubmatch(output); len(match) == 2 {
		if v, err := strconv.ParseFloat(match[1], 64); err == nil {
			lossPct = v
			haveLossSummary = true
		}
	}
	if len(rtts) > 0 && !haveLossSummary {
		// A phase-aligned ping may be cancelled as soon as its throughput
		// phase ends, before ping prints the footer. Replies already observed
		// are valid samples; do not mislabel them as 100% loss.
		lossPct = 0
	}
	transmitted := len(rtts)
	if match := pingTxRE.FindStringSubmatch(output); len(match) == 2 {
		if v, err := strconv.Atoi(match[1]); err == nil {
			transmitted = v
		}
	}
	return rtts, lossPct, transmitted, haveLossSummary
}

func computeJitter(rtts []float64) float64 {
	if len(rtts) < 2 {
		return 0
	}
	var total float64
	for i := 1; i < len(rtts); i++ {
		total += math.Abs(rtts[i] - rtts[i-1])
	}
	return total / float64(len(rtts)-1)
}

func cpuBusyPct(before, after CPUSample) float64 {
	if after.Total <= before.Total {
		return 0
	}
	if after.Busy < before.Busy {
		return 0
	}
	dBusy := after.Busy - before.Busy
	dTotal := after.Total - before.Total
	return roundTo(100*float64(dBusy)/float64(dTotal), 1)
}

func newHTTPEndpoint(rawURL string) (SpeedEndpoint, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("unsupported URL scheme: %q", rawURL)
	}
	transport := &http.Transport{
		DisableCompression:  true,
		MaxIdleConns:        1,
		MaxIdleConnsPerHost: 1,
		MaxConnsPerHost:     1,
		// Guards against a server that never answers. Deliberately NOT a
		// client-wide Timeout: that would abort an in-progress transfer
		// before its own per-request deadline.
		ResponseHeaderTimeout: speedEndpointTimeout,
	}
	return &httpEndpoint{
		u:         parsed,
		host:      parsed.Hostname(),
		client:    &http.Client{Transport: transport},
		transport: transport,
	}, nil
}

func (e *httpEndpoint) Host() string { return e.host }

func (e *httpEndpoint) Download(
	ctx context.Context,
	nbytes uint64,
	deadline time.Time,
) (uint64, time.Duration, time.Duration, error) {
	reqCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	started := time.Now()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, e.downloadURL(nbytes), nil)
	if err != nil {
		return 0, 0, 0, err
	}
	setSpeedHeaders(req)
	resp, err := e.client.Do(req)
	if err != nil {
		e.reset()
		return 0, 0, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		e.reset()
		return 0, 0, 0, fmt.Errorf("HTTP %d from %s", resp.StatusCode, e.host)
	}

	// Anchor the transfer window at header arrival, before the first body
	// read, so the first chunk's transfer time is measured rather than
	// folded into TTFB.
	bodyStart := time.Now()
	buf := make([]byte, httpChunk)
	var bytesRead uint64
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			bytesRead += uint64(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			e.reset()
			if bytesRead > 0 && (!time.Now().Before(deadline) || errors.Is(reqCtx.Err(), context.DeadlineExceeded)) {
				break
			}
			return 0, 0, 0, err
		}
		if !time.Now().Before(deadline) {
			e.reset()
			break
		}
	}

	transfer := time.Since(bodyStart)
	if transfer <= 0 {
		transfer = time.Microsecond
	}
	return bytesRead, bodyStart.Sub(started), transfer, nil
}

func (e *httpEndpoint) StreamDownload(
	ctx context.Context,
	nbytes uint64,
	deadline time.Time,
	onRead func(int) bool,
) error {
	reqCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, e.downloadURL(nbytes), nil)
	if err != nil {
		return err
	}
	setSpeedHeaders(req)
	resp, err := e.client.Do(req)
	if err != nil {
		e.reset()
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		e.reset()
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, e.host)
	}

	buf := make([]byte, httpChunk)
	remaining := nbytes
	for remaining > 0 {
		readSize := min(uint64(len(buf)), remaining)
		n, err := resp.Body.Read(buf[:readSize])
		if n > 0 {
			remaining -= uint64(n)
			if !onRead(n) {
				// Caller-requested stop mid-body: the connection can't be
				// safely reused with an unfinished response, so drop it.
				e.reset()
				return nil
			}
		}
		if remaining == 0 {
			if err != io.EOF {
				// The response may have more bytes than requested. Do not probe
				// beyond the hard cap merely to preserve keep-alive reuse.
				e.reset()
			}
			return nil
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			e.reset()
			return err
		}
		if !time.Now().Before(deadline) {
			e.reset()
			return nil
		}
	}
	return nil
}

func (e *httpEndpoint) Upload(
	ctx context.Context,
	nbytes uint64,
	deadline time.Time,
) (uint64, time.Duration, error) {
	reqCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	body := &uploadBody{
		remaining: nbytes,
		deadline:  deadline,
	}
	started := time.Now()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, e.u.String(), body)
	if err != nil {
		return 0, 0, err
	}
	req.ContentLength = int64(nbytes)
	req.Header.Set("Content-Type", "application/octet-stream")
	setSpeedHeaders(req)

	resp, err := e.client.Do(req)
	if err != nil {
		e.reset()
		elapsed := positiveDuration(time.Since(started))
		if body.sent > 0 && (errors.Is(err, errUploadDeadline) || !time.Now().Before(deadline)) {
			return body.sent, elapsed, nil
		}
		return body.sent, elapsed, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	elapsed := positiveDuration(time.Since(started))
	if resp.StatusCode >= http.StatusBadRequest {
		e.reset()
		return body.sent, elapsed, fmt.Errorf("HTTP %d from %s", resp.StatusCode, e.host)
	}
	return body.sent, elapsed, nil
}

func (e *httpEndpoint) Close() error {
	e.reset()
	return nil
}

func (e *httpEndpoint) reset() {
	if e.transport != nil {
		e.transport.CloseIdleConnections()
	}
}

func (e *httpEndpoint) downloadURL(nbytes uint64) string {
	u := *e.u
	q := u.Query()
	q.Set("bytes", strconv.FormatUint(nbytes, 10))
	u.RawQuery = q.Encode()
	return u.String()
}

func (b *uploadBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	if !time.Now().Before(b.deadline) {
		return 0, errUploadDeadline
	}
	n := min(min(uint64(len(p)), b.remaining), uint64(len(uploadPayload)))
	copy(p[:int(n)], uploadPayload[:int(n)])
	b.remaining -= n
	b.sent += n
	return int(n), nil
}

func setSpeedHeaders(req *http.Request) {
	req.Header.Set("User-Agent", buildinfo.UserAgent("network-diagnostics"))
	req.Header.Set("Accept-Encoding", "identity")
}

func newUploadPayload() []byte {
	b := make([]byte, httpChunk)
	if _, err := rand.Read(b); err == nil {
		return b
	}
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

func clampPingCount(count uint32) uint32 {
	if count == 0 {
		return defaultPingCount
	}
	if count > maxPingCount {
		return maxPingCount
	}
	return count
}

func pingProbesSent(transmitted int, fallback uint32) uint32 {
	if transmitted > 0 {
		return uint32(transmitted)
	}
	return fallback
}

func defaultString(got, fallback string) string {
	if got == "" {
		return fallback
	}
	return got
}

func roundTo(v float64, places int) float64 {
	scale := math.Pow10(places)
	return math.Round(v*scale) / scale
}

func maxFloat64(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	got := values[0]
	for _, v := range values[1:] {
		got = max(got, v)
	}
	return got
}

func minFloat64(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	got := values[0]
	for _, v := range values[1:] {
		got = min(got, v)
	}
	return got
}

func averageFloat64(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var total float64
	for _, v := range values {
		total += v
	}
	return total / float64(len(values))
}

func positiveDuration(d time.Duration) time.Duration {
	if d <= 0 {
		return time.Microsecond
	}
	return d
}
