package diagnostics

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"time"
)

const (
	phaseLatencyStopWait = 500 * time.Millisecond

	// After the phase deadline cancels the context, deadline-aware workers
	// still need a moment to unwind and report the bytes they actually
	// moved. Only workers silent past this grace are replaced with a
	// synthetic truncation result.
	bidirectionalDrainGrace = 500 * time.Millisecond
)

type (
	phaseLatencySpec struct {
		deadline time.Time
		target   string
		count    uint32
		ping     func(context.Context, string, uint32) LatencyResult
	}

	bidirectionalPhaseSpec struct {
		downloadURL     string
		uploadURL       string
		downloadBudget  uint64
		uploadBudget    uint64
		deadline        time.Time
		latency         phaseLatencySpec
		endpointFactory func(string) (SpeedEndpoint, error)
		skipLatency     bool
	}
)

func proportionalDeadline(overall time.Time, fraction float64) time.Time {
	now := time.Now()
	remaining := overall.Sub(now)
	if remaining <= 0 {
		return overall
	}
	deadline := now.Add(time.Duration(float64(remaining) * fraction))
	if deadline.After(overall) {
		return overall
	}
	return deadline
}

func splitResponsivenessBudget(total uint64, profile SpeedTestProfile) (uint64, uint64) {
	if normalizeSpeedTestProfile(profile) == SpeedTestProfileQuick {
		return total, 0
	}
	bidirectional := total / 5
	return total - bidirectional, bidirectional
}

func measureThroughputWithLatency(
	ctx context.Context,
	spec phaseLatencySpec,
	measure func(context.Context) ThroughputResult,
) (ThroughputResult, LatencyResult) {
	phaseCtx, cancel := context.WithDeadline(ctx, spec.deadline)
	// measure is caller-supplied; the defer keeps the context and ping
	// goroutine from leaking if it panics. Double cancel is safe.
	defer cancel()
	latencyDone := make(chan LatencyResult, 1)
	latencyStarted := make(chan struct{})
	go func() {
		close(latencyStarted)
		latencyDone <- safePing(phaseCtx, spec)
	}()
	<-latencyStarted

	throughput := measure(phaseCtx)
	cancel()

	wait := min(phaseLatencyStopWait, max(time.Until(spec.deadline), 0))
	if wait == 0 {
		select {
		case latency := <-latencyDone:
			latency.Target = spec.target
			return throughput, latency
		default:
			return throughput, LatencyResult{
				Target: spec.target,
				Error:  "phase latency sampler reached phase deadline",
			}
		}
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case latency := <-latencyDone:
		latency.Target = spec.target
		return throughput, latency
	case <-timer.C:
		return throughput, LatencyResult{
			Target: spec.target,
			Error:  "phase latency sampler did not stop after cancellation",
		}
	}
}

func safePing(ctx context.Context, spec phaseLatencySpec) (result LatencyResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = LatencyResult{Error: fmt.Sprintf("phase ping raised: %v", recovered)}
		}
	}()
	return spec.ping(ctx, spec.target, spec.count)
}

func runUploadPhase(
	ctx context.Context,
	endpoint SpeedEndpoint,
	budget uint64,
	deadline time.Time,
) ThroughputResult {
	return runRamp(
		ctx,
		func(
			ctx context.Context,
			nbytes uint64,
			deadline time.Time,
		) (uint64, time.Duration, time.Duration, error) {
			moved, transfer, err := endpoint.Upload(ctx, nbytes, deadline)
			return moved, 0, transfer, err
		},
		uploadStages,
		budget,
		deadline,
	)
}

func runBidirectionalPhase(
	ctx context.Context,
	spec bidirectionalPhaseSpec,
) (ThroughputResult, ThroughputResult, LatencyResult) {
	if spec.downloadBudget == 0 || spec.uploadBudget == 0 {
		// A byte budget too small to split is a capped skip, not a phase
		// failure — the "partial:" prefix keeps it out of the failed-phase
		// count while still explaining the missing measurements.
		errText := "partial: bidirectional byte budget unavailable"
		return ThroughputResult{CapHit: true, Error: errText},
			ThroughputResult{CapHit: true, Error: errText},
			LatencyResult{Error: errText}
	}

	downloadEndpoint, err := spec.endpointFactory(spec.downloadURL)
	if err != nil {
		errText := "bidirectional download endpoint: " + err.Error()
		return ThroughputResult{Error: errText}, ThroughputResult{Error: "not run"}, LatencyResult{Error: errText}
	}
	uploadEndpoint, err := spec.endpointFactory(spec.uploadURL)
	if err != nil {
		_ = downloadEndpoint.Close()
		errText := "bidirectional upload endpoint: " + err.Error()
		return ThroughputResult{Error: "not run"}, ThroughputResult{Error: errText}, LatencyResult{Error: errText}
	}
	// The drain grace lets this function return while a worker is still
	// inside StreamDownload/Upload; closing an endpoint under it would race.
	// Hand cleanup to a goroutine that waits for both workers to return —
	// they observe phaseCtx cancellation, so the wait is bounded.
	var workers sync.WaitGroup
	defer func() {
		go func() {
			workers.Wait()
			_ = downloadEndpoint.Close()
			_ = uploadEndpoint.Close()
		}()
	}()

	var download ThroughputResult
	var upload ThroughputResult
	measure := func(phaseCtx context.Context) ThroughputResult {
		downloadDone := make(chan ThroughputResult, 1)
		uploadDone := make(chan ThroughputResult, 1)
		workers.Add(2)
		go func() {
			defer workers.Done()
			downloadDone <- safeThroughputPhase("bidirectional download", func() ThroughputResult {
				return measureDownloadStream(
					phaseCtx,
					downloadEndpoint,
					spec.deadline,
					spec.downloadBudget,
				)
			})
		}()
		go func() {
			defer workers.Done()
			uploadDone <- safeThroughputPhase("bidirectional upload", func() ThroughputResult {
				return runUploadPhase(
					phaseCtx,
					uploadEndpoint,
					spec.uploadBudget,
					spec.deadline,
				)
			})
		}()

		for downloadDone != nil || uploadDone != nil {
			select {
			case download = <-downloadDone:
				downloadDone = nil
			case upload = <-uploadDone:
				uploadDone = nil
			case <-phaseCtx.Done():
				// The workers observe the same deadline and report real
				// partial measurements (bytes moved, CapHit) shortly after
				// cancellation; drain those instead of discarding them.
				grace := time.NewTimer(bidirectionalDrainGrace)
				defer grace.Stop()
				for downloadDone != nil || uploadDone != nil {
					select {
					case download = <-downloadDone:
						downloadDone = nil
					case upload = <-uploadDone:
						uploadDone = nil
					case <-grace.C:
						if downloadDone != nil {
							download = truncatedPhaseResult("bidirectional download", phaseCtx.Err())
						}
						if uploadDone != nil {
							upload = truncatedPhaseResult("bidirectional upload", phaseCtx.Err())
						}
						return ThroughputResult{}
					}
				}
				return ThroughputResult{}
			}
		}
		return ThroughputResult{}
	}

	if spec.skipLatency {
		phaseCtx, cancel := context.WithDeadline(ctx, spec.deadline)
		defer cancel()
		measure(phaseCtx)
		return download, upload, LatencyResult{}
	}
	_, latency := measureThroughputWithLatency(ctx, spec.latency, measure)
	return download, upload, latency
}

func safeThroughputPhase(phase string, measure func() ThroughputResult) (result ThroughputResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result = ThroughputResult{Error: fmt.Sprintf("%s raised: %v", phase, recovered)}
		}
	}()
	return measure()
}

func truncatedPhaseResult(phase string, err error) ThroughputResult {
	return ThroughputResult{
		CapHit: true,
		Error:  fmt.Sprintf("%s did not stop before deadline: %v", phase, err),
	}
}

func bidirectionalEndpointHost(downloadURL, uploadURL string) string {
	for _, rawURL := range []string{downloadURL, uploadURL} {
		parsed, err := url.Parse(rawURL)
		if err == nil && parsed.Hostname() != "" {
			return parsed.Hostname()
		}
	}
	return ""
}
