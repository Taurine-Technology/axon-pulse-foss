package qualitytest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Taurine-Technology/axon-pulse/internal/protocol"
	"github.com/Taurine-Technology/axon-pulse/quality"
)

type (
	zeroReader     struct{}
	probeTransport func(*http.Request) (*http.Response, error)
)

func TestRunnerUsesRealBucketsAndSeparatePathProbesWithinBudget(t *testing.T) {
	t.Parallel()
	var connectionsMu sync.Mutex
	probeConnections := map[string]bool{}
	bulkConnections := map[string]bool{}
	connectionRequests := map[string][]string{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Server-Timing", "app;dur=0.1")
		switch request.URL.Path {
		case "/down":
			amount, _ := strconv.ParseInt(request.URL.Query().Get("bytes"), 10, 64)
			connectionsMu.Lock()
			if amount == 0 {
				probeConnections[request.RemoteAddr] = true
			} else {
				bulkConnections[request.RemoteAddr] = true
			}
			connectionRequests[request.RemoteAddr] = append(connectionRequests[request.RemoteAddr], request.URL.String())
			connectionsMu.Unlock()
			if amount > 0 {
				_, _ = io.CopyN(writer, zeroReader{}, amount)
			}
		case "/up":
			connectionsMu.Lock()
			bulkConnections[request.RemoteAddr] = true
			connectionRequests[request.RemoteAddr] = append(connectionRequests[request.RemoteAddr], request.URL.String())
			connectionsMu.Unlock()
			_, _ = io.Copy(io.Discard, request.Body)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	profile := protocol.SpeedProfileConfig{
		Name: "content", Enabled: true, MinDurationSeconds: 2, MaxDurationSeconds: 8,
		MaxDownloadBytes: 1 << 20, MaxUploadBytes: 512 << 10, MaxConcurrentRequests: 2,
		CountsTowardExperience: true,
	}
	var progressMu sync.Mutex
	progressPhases := map[string]bool{}
	progressDone := false
	result, err := (Runner{}).Run(context.Background(), Options{
		Profile: profile, DownloadURL: server.URL + "/down", UploadURL: server.URL + "/up", Client: server.Client(),
		Traffic: protocol.TrafficContext{Contaminated: true, Override: true},
		OnProgress: func(p protocol.SpeedTestProgress) {
			progressMu.Lock()
			progressPhases[p.Phase] = true
			progressDone = progressDone || p.Done
			progressMu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	progressMu.Lock()
	for _, phase := range []string{"baseline", "download", "upload", "done"} {
		if !progressPhases[phase] {
			t.Fatalf("live progress never reported phase %q: %v", phase, progressPhases)
		}
	}
	if !progressDone {
		t.Fatal("live progress never reported completion")
	}
	progressMu.Unlock()
	if result.MeasurementID == "" || result.Profile != "content" || !result.ExperienceEligible {
		t.Fatalf("identity/profile = %+v", result)
	}
	if result.Budget.UsedBytes == 0 || result.Budget.UsedBytes > profile.MaxDownloadBytes+profile.MaxUploadBytes {
		t.Fatalf("data use escaped budget: %+v", result.Budget)
	}
	if len(result.Phases["download"].Buckets) == 0 || len(result.Phases["upload"].Buckets) == 0 {
		t.Fatalf("missing real phase buckets: %+v", result.Phases)
	}
	if result.Phases["baseline"].EndMS > result.Phases["download_warmup"].StartMS || result.Phases["download_warmup"].EndMS > result.Phases["upload_warmup"].StartMS {
		t.Fatalf("phase boundaries overlap: %+v", result.Phases)
	}
	if result.Paths.Persistent.Samples == 0 || result.Paths.Fresh.Samples == 0 || result.Paths.Fresh.Successes == 0 {
		t.Fatalf("path probes = %+v", result.Paths)
	}
	if result.Responsiveness.Baseline.Count != quality.MinimumBaselineSamples {
		t.Fatalf("runner did not collect the full idle baseline: %+v", result.Responsiveness.Baseline)
	}
	connectionsMu.Lock()
	for address := range probeConnections {
		if bulkConnections[address] {
			connectionsMu.Unlock()
			t.Fatalf("latency and bulk traffic shared connection %s (requests=%v)", address, connectionRequests)
		}
	}
	connectionsMu.Unlock()
	if len(result.Experience.Home) != 4 || result.MeasurementConfidence.Level == "" {
		t.Fatalf("experience/confidence = %+v / %+v", result.Experience, result.MeasurementConfidence)
	}
	foundContamination := false
	for _, reason := range result.MeasurementConfidence.Reasons {
		foundContamination = foundContamination || reason == "cross_traffic_detected"
	}
	if !result.TrafficContext.Contaminated || !result.TrafficContext.Override || !foundContamination {
		t.Fatalf("override lost contamination evidence: traffic=%+v confidence=%+v", result.TrafficContext, result.MeasurementConfidence)
	}
	// Loopback exhausts these byte budgets long before MinDurationSeconds, so
	// the result must name the budget as the cause rather than the network.
	for _, phase := range []string{"download", "upload"} {
		if result.Phases[phase].StableRegion.Stable {
			continue
		}
		if !slices.Contains(result.MeasurementConfidence.Reasons, phase+"_budget_limited") {
			t.Fatalf("capped unstable %s phase missing budget_limited reason: %+v", phase, result.MeasurementConfidence.Reasons)
		}
	}
}

func TestOneTransferHonorsDownloadCeilingWhenEndpointOversends(t *testing.T) {
	t.Parallel()
	const budget = uint64(128 << 10)
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.CopyN(writer, zeroReader{}, int64(2*budget))
	}))
	defer server.Close()

	result := oneTransfer(context.Background(), server.Client(), "download", server.URL, budget, nil, nil)
	if result.Error != "" {
		t.Fatal(result.Error)
	}
	if result.BytesTransferred != budget {
		t.Fatalf("download transferred %d bytes, want hard ceiling %d", result.BytesTransferred, budget)
	}
}

func TestRunnerDoesNotPersistMeaninglessResultAfterParentCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	profile := protocol.SpeedProfileConfig{Name: "content", MinDurationSeconds: 2, MaxDurationSeconds: 8, MaxDownloadBytes: 1 << 20, MaxUploadBytes: 512 << 10, MaxConcurrentRequests: 2}
	if _, err := (Runner{}).Run(ctx, Options{Profile: profile}); err == nil {
		t.Fatal("canceled test returned a completed result")
	}
}

func TestRunnerReturnsErrorForEndpointPhaseFailures(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	profile := protocol.SpeedProfileConfig{
		Name: "content", MinDurationSeconds: 2, MaxDurationSeconds: 8,
		MaxDownloadBytes: 1 << 20, MaxUploadBytes: 512 << 10, MaxConcurrentRequests: 2,
	}
	result, err := (Runner{}).Run(context.Background(), Options{
		Profile: profile, DownloadURL: server.URL + "/down", UploadURL: server.URL + "/up", Client: server.Client(),
	})
	if err == nil || (result.Download.Error == "" && result.Upload.Error == "") {
		t.Fatalf("endpoint failure result=%+v error=%v", result, err)
	}
}

func TestProbeBoundsAndAccountsChunkedResponseBodies(t *testing.T) {
	t.Parallel()
	const capacity = 8 << 10
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		flusher, ok := writer.(http.Flusher)
		if !ok {
			return
		}
		// Unambiguously exceed the budget rather than exactly filling it.
		for range 9 {
			_, _ = writer.Write(make([]byte, 1024))
			flusher.Flush()
		}
	}))
	defer server.Close()
	budget := newResponseBudget(capacity)
	if _, ok, _ := probeOnce(context.Background(), server.Client(), server.URL, budget); ok {
		t.Fatal("oversized chunked probe response was accepted")
	}
	if got := budget.used.Load(); got == 0 || got > capacity {
		t.Fatalf("accounted response bytes=%d, want bounded non-zero use", got)
	}
}

func TestProbeBudgetExhaustionIsNotPacketLoss(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("ok"))
	}))
	defer server.Close()
	exhausted := newResponseBudget(0)
	if _, ok, budgetExhausted := probeOnce(context.Background(), server.Client(), server.URL, exhausted); ok || !budgetExhausted {
		t.Fatalf("exhausted budget probe ok=%v budgetExhausted=%v, want failure flagged as exhaustion", ok, budgetExhausted)
	}
	summary, paths := collectFixedLatency(context.Background(), server.Client(), server.URL, 5, exhausted, nil)
	if summary.Attempts != 0 || summary.PacketLossValid || paths.persistentAttempts != 0 {
		t.Fatalf("budget exhaustion counted as loss: %+v paths=%+v", summary, paths)
	}
}

func TestQualityClientRejectsRedirects(t *testing.T) {
	t.Parallel()
	var followed atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/redirect" {
			http.Redirect(writer, request, "/private", http.StatusFound)
			return
		}
		followed.Store(true)
	}))
	defer server.Close()
	client := cloneClient(server.Client(), false, 1)
	result := oneTransfer(context.Background(), client, "download", server.URL+"/redirect", 128<<10, nil, nil)
	if result.Error == "" || followed.Load() {
		t.Fatalf("redirect was followed: result=%+v followed=%v", result, followed.Load())
	}
}

func TestAdaptiveAllocationScalesWithRateAndRespectsCeiling(t *testing.T) {
	t.Parallel()
	const ceiling = uint64(256 << 20)
	window := 12 * time.Second
	if got := adaptiveAllocation(0, window, ceiling); got != ceiling {
		t.Fatalf("unknown rate should fall back to ceiling, got %d", got)
	}
	slow := adaptiveAllocation(8, window, ceiling) // 8 Mbps for 12s ≈ 12 MB × 1.5
	if slow < 12<<20 || slow > 24<<20 {
		t.Fatalf("slow-link allocation = %d bytes", slow)
	}
	fast := adaptiveAllocation(150, window, ceiling) // 150 Mbps needs ~337 MB > ceiling
	if fast != ceiling {
		t.Fatalf("fast-link allocation should clamp to ceiling, got %d", fast)
	}
	if tiny := adaptiveAllocation(0.01, window, ceiling); tiny < 1<<20 {
		t.Fatalf("allocation floor lost: %d", tiny)
	}
}

func TestWarmupRateRecoversEstimateFromInterruptedTransfer(t *testing.T) {
	t.Parallel()
	if got := warmupRate(protocol.ThroughputResult{ThroughputMbps: 42}); got != 42 {
		t.Fatalf("clean rate = %v", got)
	}
	interrupted := protocol.ThroughputResult{BytesTransferred: 1 << 20, DurationMS: 1000, Error: "context deadline exceeded"}
	if got := warmupRate(interrupted); got < 8.3 || got > 8.5 { // 1 MiB in 1s ≈ 8.39 Mbps
		t.Fatalf("interrupted rate = %v", got)
	}
	if got := warmupRate(protocol.ThroughputResult{Error: "dial failed"}); got != 0 {
		t.Fatalf("failed transfer should have no rate, got %v", got)
	}
}

func TestBoundedLoadAdvertisesOnlyFundedConcurrentStreams(t *testing.T) {
	t.Parallel()
	streams, block := boundedLoad(512<<10, 8, 8<<20)
	if streams != 8 || block > (512<<10)/8 {
		t.Fatalf("bounded load streams=%d block=%d", streams, block)
	}
	streams, block = boundedLoad(128<<10, 8, 8<<20)
	if streams != 2 || block != 64<<10 {
		t.Fatalf("small bounded load streams=%d block=%d", streams, block)
	}
}

func TestFixedLatencyCancellationBeforeProbeIsNotPacketLoss(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	summary, paths := collectFixedLatency(ctx, http.DefaultClient, "https://example.invalid", 20, newResponseBudget(8<<10), nil)
	if summary.Attempts != 0 || summary.Count != 0 || summary.PacketLossValid || paths.persistentAttempts != 0 {
		t.Fatalf("canceled fixed probes = %+v paths=%+v", summary, paths)
	}
}

func TestLoadedLatencyCancellationInFlightIsNotPacketLoss(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	var startedOnce sync.Once
	server := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		startedOnce.Do(func() { close(started) })
		<-request.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	paths := pathSamples{}
	summary := collectLoadedLatency(ctx, server.Client(), server.URL, &paths, newResponseBudget(8<<10), nil)
	if summary.Attempts != 0 || summary.Count != 0 || summary.PacketLossValid || paths.persistentAttempts != 0 {
		t.Fatalf("in-flight cancellation counted as loss: %+v paths=%+v", summary, paths)
	}
}

func (zeroReader) Read(buffer []byte) (int, error) {
	clear(buffer)
	return len(buffer), nil
}

func TestBaselineSeparatesSetupAndStillDetectsSustainedLatency(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		spike      bool
		slow       bool
		warmupFail bool
		rating     quality.Rating
	}{
		{name: "slow initial connection", rating: quality.RatingGood},
		{name: "one later spike", spike: true, rating: quality.RatingGood},
		{name: "sustained slow requests", slow: true, rating: quality.RatingBad},
		{name: "failed warmup remains in diagnostics", warmupFail: true, rating: quality.RatingGood},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				requests := 0
				client := &http.Client{Transport: probeTransport(func(request *http.Request) (*http.Response, error) {
					requests++
					delay := 80 * time.Millisecond
					if test.slow {
						delay = 214 * time.Millisecond
					}
					if test.spike && requests == 8 {
						delay = 286 * time.Millisecond
					}
					status := http.StatusOK
					if requests == 1 {
						delay = 500 * time.Millisecond
						if test.warmupFail {
							status = http.StatusServiceUnavailable
						}
					}
					time.Sleep(delay)
					return &http.Response{
						StatusCode: status, Header: http.Header{}, Request: request,
						Body: io.NopCloser(strings.NewReader("ok")), ContentLength: 2,
					}, nil
				})}
				responses := newResponseBudget(8 << 10)
				rtts := []float64{}
				started := time.Now()
				baseline, paths := collectBaselineLatency(
					t.Context(), client, "https://example.invalid/down", responses,
					func(rtt float64) { rtts = append(rtts, rtt) },
				)
				if baseline.Count != 20 || baseline.Attempts != 20 || len(rtts) != 20 {
					t.Fatalf("idle evidence = %+v, live samples=%d", baseline, len(rtts))
				}
				if baseline.MaxMS >= 500 || slices.Contains(rtts, 500) {
					t.Fatalf("initial connection polluted idle evidence: %+v", baseline)
				}
				if test.spike && baseline.MaxMS != 286 {
					t.Fatalf("later latency spike was discarded: %+v", baseline)
				}
				if !test.warmupFail && !slices.Contains(paths.fresh, 500) {
					t.Fatalf("initial connection missing from fresh diagnostics: %+v", paths)
				}
				if test.warmupFail && paths.freshSuccessRatio() >= 1 {
					t.Fatal("warmup failure was hidden from path confidence")
				}
				if responses.used.Load() != uint64(2*requests) {
					t.Fatalf("warmup/probe bytes unaccounted: used=%d requests=%d", responses.used.Load(), requests)
				}
				if !test.slow && time.Since(started) > 4*time.Second {
					t.Fatalf("idle sampling needlessly extended the test: %v", time.Since(started))
				}
				score := quality.ScoreApplications(quality.ApplicationInput{
					Baseline:        phaseMetrics(&baseline),
					DownloadP75Mbps: 100, UploadP75Mbps: 90, DownloadAvailable: true, UploadAvailable: true,
				}).Home["browsing"]
				if score.Rating != test.rating {
					t.Fatalf("browsing = %+v, want %s", score, test.rating)
				}
			})
		})
	}
}

func TestBaselineHonorsCancellationAndResponseBudget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		deadline time.Duration
		capacity uint64
	}{
		{name: "already canceled", deadline: 0, capacity: 8192},
		{name: "canceled during warmup", deadline: 50 * time.Millisecond, capacity: 8192},
		{name: "canceled during sampling", deadline: 500 * time.Millisecond, capacity: 8192},
		{name: "response budget exhausted", deadline: time.Second, capacity: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				client := &http.Client{Transport: probeTransport(func(request *http.Request) (*http.Response, error) {
					select {
					case <-request.Context().Done():
						return nil, request.Context().Err()
					case <-time.After(100 * time.Millisecond):
						return &http.Response{
							StatusCode: http.StatusOK, Header: http.Header{}, Request: request,
							Body: io.NopCloser(strings.NewReader("ok")), ContentLength: 2,
						}, nil
					}
				})}
				ctx, cancel := context.WithTimeout(t.Context(), test.deadline)
				defer cancel()
				responses := newResponseBudget(test.capacity)
				baseline, paths := collectBaselineLatency(ctx, client, "https://example.invalid", responses, nil)
				pathLoss := paths.freshAttempts > 0 && paths.successRatio() != 1
				if baseline.PacketLossPct != 0 || pathLoss {
					t.Fatalf("local limit reported as loss: %+v paths=%+v", baseline, paths)
				}
				if responses.used.Load() > test.capacity {
					t.Fatal("baseline exceeded the response budget")
				}
				score := quality.ScoreApplications(quality.ApplicationInput{
					Baseline:        phaseMetrics(&baseline),
					DownloadP75Mbps: 100, UploadP75Mbps: 90, DownloadAvailable: true, UploadAvailable: true,
				}).Home["browsing"]
				if score.Available || !slices.Contains(score.UnavailableReasons, "insufficient_baseline_samples") {
					t.Fatalf("incomplete baseline produced a verdict: %+v", score)
				}
			})
		})
	}
}

func (transport probeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}
