package diagnostics

import (
	"bytes"
	"context"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParsePingOutputAndJitter(t *testing.T) {
	tests := []struct {
		name          string
		output        string
		wantRTTs      []float64
		wantLoss      float64
		wantTx        int
		wantLossValid bool
		wantJitter    float64
	}{
		{
			name: "iputils",
			output: strings.Join([]string{
				"64 bytes from 1.1.1.1: icmp_seq=1 ttl=57 time=10.0 ms",
				"64 bytes from 1.1.1.1: icmp_seq=2 ttl=57 time=14.5 ms",
				"64 bytes from 1.1.1.1: icmp_seq=3 ttl=57 time=12.5 ms",
				"3 packets transmitted, 3 received, 0% packet loss, time 400ms",
			}, "\n"),
			wantRTTs:      []float64{10.0, 14.5, 12.5},
			wantLoss:      0,
			wantTx:        3,
			wantLossValid: true,
			wantJitter:    3.25,
		},
		{
			name: "busybox with loss",
			output: strings.Join([]string{
				"64 bytes from 192.0.2.1: seq=0 ttl=64 time<1 ms",
				"2 packets transmitted, 1 packets received, 50% packet loss",
			}, "\n"),
			wantRTTs:      []float64{1},
			wantLoss:      50,
			wantTx:        2,
			wantLossValid: true,
			wantJitter:    0,
		},
		{
			name: "phase cancellation before footer keeps observed replies",
			output: strings.Join([]string{
				"64 bytes from 1.1.1.1: icmp_seq=1 ttl=57 time=10.0 ms",
				"64 bytes from 1.1.1.1: icmp_seq=2 ttl=57 time=12.0 ms",
			}, "\n"),
			wantRTTs:      []float64{10, 12},
			wantLoss:      0,
			wantTx:        2,
			wantLossValid: false,
			wantJitter:    2,
		},
		{
			name:       "no summary defaults to total loss",
			output:     "",
			wantRTTs:   []float64{},
			wantLoss:   100,
			wantTx:     0,
			wantJitter: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rtts, loss, transmitted, lossValid := parsePingOutput(tt.output)
			if !slices.Equal(rtts, tt.wantRTTs) ||
				loss != tt.wantLoss ||
				transmitted != tt.wantTx ||
				lossValid != tt.wantLossValid {
				t.Fatalf("parsePingOutput = (%v, %.3f, %d, %v), want (%v, %.3f, %d, %v)",
					rtts, loss, transmitted, lossValid,
					tt.wantRTTs, tt.wantLoss, tt.wantTx, tt.wantLossValid)
			}
			if got := computeJitter(rtts); math.Abs(got-tt.wantJitter) > 0.0001 {
				t.Fatalf("computeJitter(%v) = %.4f, want %.4f", rtts, got, tt.wantJitter)
			}
		})
	}
}

func TestRunRampStopConditions(t *testing.T) {
	tests := []struct {
		name       string
		stages     []uint64
		budget     uint64
		deadline   time.Time
		moved      []uint64
		durations  []time.Duration
		wantCalls  int
		wantBytes  uint64
		wantCapHit bool
		wantError  string
	}{
		{
			name: "byte budget reached is a complete sample (not capped)",
			// Reaching the byte budget means a full, accurate measurement —
			// CapHit is reserved for time-truncation.
			stages:     []uint64{200, 200},
			budget:     200,
			deadline:   time.Now().Add(time.Minute),
			moved:      []uint64{200},
			durations:  []time.Duration{100 * time.Millisecond},
			wantCalls:  1,
			wantBytes:  200,
			wantCapHit: false,
		},
		{
			name:       "past deadline stops before transfer",
			stages:     []uint64{200},
			budget:     1000,
			deadline:   time.Now().Add(-time.Millisecond),
			wantCalls:  0,
			wantBytes:  0,
			wantCapHit: true,
			wantError:  "no measurement completed",
		},
		{
			name: "ramp does not stop early on stable-looking burst samples",
			// Two consecutive samples within the old 10% stability band no
			// longer short-circuit the ramp — an ISP burst looks stable, so
			// stopping there would lock onto the burst rate. It runs to the
			// byte budget instead.
			stages:     []uint64{200_000, 200_000, 200_000},
			budget:     600_000,
			deadline:   time.Now().Add(time.Minute),
			moved:      []uint64{200_000, 200_000, 200_000},
			durations:  []time.Duration{time.Millisecond, 1050 * time.Microsecond, time.Millisecond},
			wantCalls:  3,
			wantBytes:  600_000,
			wantCapHit: false,
		},
		{
			name:       "partial stage marks time truncation",
			stages:     []uint64{1000},
			budget:     10_000,
			deadline:   time.Now().Add(time.Minute),
			moved:      []uint64{400},
			durations:  []time.Duration{time.Millisecond},
			wantCalls:  1,
			wantBytes:  400,
			wantCapHit: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls int
			result := runRamp(context.Background(), func(ctx context.Context, nbytes uint64, deadline time.Time) (uint64, time.Duration, time.Duration, error) {
				_ = ctx
				_ = deadline
				if calls >= len(tt.moved) {
					t.Fatalf("unexpected transfer call %d for %d bytes", calls+1, nbytes)
				}
				moved := tt.moved[calls]
				duration := tt.durations[calls]
				calls++
				return moved, 2 * time.Millisecond, duration, nil
			}, tt.stages, tt.budget, tt.deadline)

			if calls != tt.wantCalls {
				t.Fatalf("transfer calls = %d, want %d", calls, tt.wantCalls)
			}
			if result.BytesTransferred != tt.wantBytes ||
				result.CapHit != tt.wantCapHit ||
				result.Error != tt.wantError {
				t.Fatalf("result = %+v, want bytes=%d cap=%v error=%q",
					result, tt.wantBytes, tt.wantCapHit, tt.wantError)
			}
		})
	}
}

func TestSustainedThroughputMbps(t *testing.T) {
	tests := []struct {
		name    string
		samples []float64
		want    float64
	}{
		{"empty", nil, 0},
		{"single sample returned as-is", []float64{300}, 300},
		{"two samples take the tail", []float64{600, 300}, 300},
		{
			// The classic burst: small early chunks clock 600+ before the
			// shaper engages; the sustained rate is ~300. Peak (620) and
			// even the whole-set mean (~430) overstate it — the tail nails it.
			name:    "burst ramp collapses to the sustained tail",
			samples: []float64{620, 590, 305, 300, 298, 302},
			want:    300, // avg of the last three: (300+298+302)/3
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sustainedThroughputMbps(tt.samples)
			if math.Abs(got-tt.want) > 0.01 {
				t.Fatalf("sustainedThroughputMbps(%v) = %v, want %v", tt.samples, got, tt.want)
			}
		})
	}
}

func TestRunRampReportsSustainedNotPeak(t *testing.T) {
	// A 300 Mbps line that bursts to ~600 for the first few MB. Chunk
	// rate = bytes*8/duration/1e6, so 1 MiB in 14ms ≈ 600 Mbps (burst)
	// and 5 MiB in 140ms ≈ 300 Mbps (shaped). The reported figure must be
	// the shaped ~300, NOT the 600 burst peak the old max()-of-samples
	// logic produced.
	const mib = 1024 * 1024
	moved := []uint64{mib, 5 * mib, 5 * mib, 5 * mib, 5 * mib}
	durations := []time.Duration{
		14 * time.Millisecond,  // 1 MiB burst ≈ 599 Mbps
		140 * time.Millisecond, // 5 MiB shaped ≈ 299 Mbps
		140 * time.Millisecond,
		140 * time.Millisecond,
		140 * time.Millisecond,
	}
	var calls int
	result := runRamp(context.Background(), func(ctx context.Context, nbytes uint64, deadline time.Time) (uint64, time.Duration, time.Duration, error) {
		_ = ctx
		_ = deadline
		if calls >= len(moved) {
			t.Fatalf("unexpected transfer call %d", calls+1)
		}
		m, d := moved[calls], durations[calls]
		calls++
		return m, 2 * time.Millisecond, d, nil
	}, []uint64{1 * mib, 5 * mib}, 21*mib, time.Now().Add(time.Minute))

	if result.CapHit {
		t.Fatalf("byte budget reached should not be CapHit: %+v", result)
	}
	if result.ThroughputMbps < 250 || result.ThroughputMbps > 340 {
		t.Fatalf("throughput = %v Mbps, want the shaped ~300 (not the 600 burst peak)", result.ThroughputMbps)
	}
}

type (
	// rateStreamEndpoint delivers bytes at a configurable Mbps, optionally
	// with an initial burst at a higher rate for the first burstBytes — the
	// exact shape of an ISP token-bucket line (fast burst, then shaped).
	rateStreamEndpoint struct {
		shapedMbps float64
		burstMbps  float64
		burstBytes uint64
		totalSent  uint64
	}
	fakeSpeedEndpoint struct {
		host string
	}
	countingReader struct {
		reader *bytes.Reader
		read   int
	}
	roundTripFunc func(*http.Request) (*http.Response, error)
)

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read += n
	return n, err
}

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

func (e *rateStreamEndpoint) Host() string { return "rate-host" }
func (e *rateStreamEndpoint) Download(context.Context, uint64, time.Time) (uint64, time.Duration, time.Duration, error) {
	return 0, 0, 0, nil
}
func (e *rateStreamEndpoint) Upload(context.Context, uint64, time.Time) (uint64, time.Duration, error) {
	return 0, 0, nil
}
func (e *rateStreamEndpoint) Close() error { return nil }

func (e *rateStreamEndpoint) StreamDownload(ctx context.Context, nbytes uint64, deadline time.Time, onRead func(int) bool) error {
	_ = ctx
	// Pace delivery against ACTUAL elapsed time each tick, so sleep jitter
	// doesn't distort the effective rate the caller measures.
	last := time.Now()
	var requestSent uint64
	for {
		if requestSent >= nbytes {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
		now := time.Now()
		dt := now.Sub(last).Seconds()
		last = now
		mbps := e.shapedMbps
		if e.totalSent < e.burstBytes && e.burstMbps > 0 {
			mbps = e.burstMbps
		}
		want := min(uint64(mbps*1e6/8*dt), nbytes-requestSent)
		for want > 0 {
			n := min(want, uint64(65536), nbytes-requestSent)
			requestSent += n
			e.totalSent += n
			want -= n
			if !onRead(int(n)) {
				return nil
			}
		}
		if !now.Before(deadline) {
			return nil
		}
	}
}

func TestMeasureDownloadStreamReportsShapedRate(t *testing.T) {
	// A line that bursts at 600 Mbps for the first 30 MB, then shapes to
	// 300 — the real token-bucket shape that made the old per-chunk logic
	// report ~500. Wall-clock tail sampling must land on the shaped ~300.
	endpoint := &rateStreamEndpoint{shapedMbps: 300, burstMbps: 600, burstBytes: 30 * 1000 * 1000}
	result := measureDownloadStream(
		context.Background(), endpoint, time.Now().Add(10*time.Second), 400*1000*1000,
	)
	if result.CapHit {
		t.Fatalf("clean run should not be CapHit: %+v", result)
	}
	if result.ThroughputMbps < 270 || result.ThroughputMbps > 330 {
		t.Fatalf("throughput = %.1f Mbps, want the shaped ~300 (burst discarded)", result.ThroughputMbps)
	}
}

func TestMeasureDownloadStreamDeadlineIsFloor(t *testing.T) {
	// A slow steady line cut short by the deadline before the warmup window
	// completes reports the whole transfer, flagged CapHit (a floor).
	endpoint := &rateStreamEndpoint{shapedMbps: 50}
	result := measureDownloadStream(
		context.Background(), endpoint, time.Now().Add(600*time.Millisecond), 400*1000*1000,
	)
	if !result.CapHit {
		t.Fatalf("deadline-truncated run should be CapHit (floor): %+v", result)
	}
	if result.BytesTransferred == 0 {
		t.Fatalf("expected some bytes measured: %+v", result)
	}
}

func TestRunSpeedTestWithFakeEndpoint(t *testing.T) {
	factory := func(rawURL string) (SpeedEndpoint, error) {
		return &fakeSpeedEndpoint{host: rawURL}, nil
	}
	pings := []LatencyResult{
		{
			MinMS: 10, AvgMS: 11, MaxMS: 12, JitterMS: 1, PacketLossPct: 0, ProbesSent: 3,
			PacketLossValid: true, SamplesMS: []float64{10, 11, 12},
		},
		{
			MinMS: 20, AvgMS: 21.1234, MaxMS: 22, JitterMS: 1, PacketLossPct: 0,
			PacketLossValid: true, ProbesSent: loadedPingCount, SamplesMS: []float64{20, 21.1234, 22},
		},
		{
			MinMS: 30, AvgMS: 31, MaxMS: 32, JitterMS: 1, PacketLossPct: 0,
			PacketLossValid: true, ProbesSent: loadedPingCount, SamplesMS: []float64{30, 31, 32},
		},
	}
	var pingCalls int
	var pingMu sync.Mutex
	var pingErr string
	cpuSamples := []CPUSample{
		{Busy: 100, Total: 1000},
		{Busy: 150, Total: 1100},
	}
	var cpuCalls int

	result := RunSpeedTest(context.Background(), SpeedTestOptions{
		DownloadURL:        "download-host",
		UploadURL:          "upload-host",
		PingTarget:         "192.0.2.1",
		MaxDurationSeconds: 5,
		MaxDownloadBytes:   300_000,
		MaxUploadBytes:     300_000,
		PingCount:          3,
		EndpointFactory:    factory,
		Ping: func(ctx context.Context, target string, count uint32) LatencyResult {
			_ = ctx
			pingMu.Lock()
			defer pingMu.Unlock()
			if target != "192.0.2.1" {
				pingErr = "unexpected ping target " + target
				return LatencyResult{Error: pingErr}
			}
			if pingCalls >= len(pings) {
				pingErr = "unexpected extra ping call"
				return LatencyResult{Error: pingErr}
			}
			got := pings[pingCalls]
			pingCalls++
			if pingCalls == 1 && count != 3 {
				pingErr = "unexpected idle ping count"
				return LatencyResult{Error: pingErr}
			}
			return got
		},
		CPUReader: func() (CPUSample, bool) {
			if cpuCalls >= len(cpuSamples) {
				t.Fatalf("unexpected CPU read %d", cpuCalls+1)
			}
			got := cpuSamples[cpuCalls]
			cpuCalls++
			return got, true
		},
	})

	pingMu.Lock()
	gotPingErr := pingErr
	pingMu.Unlock()
	if gotPingErr != "" {
		t.Fatal(gotPingErr)
	}
	if result.Download.BytesTransferred == 0 || result.Upload.BytesTransferred == 0 {
		t.Fatalf("throughput not measured: %+v", result)
	}
	if result.EndpointHost != "download-host" {
		t.Fatalf("endpoint host = %q, want download-host", result.EndpointHost)
	}
	if result.Latency.Target != "192.0.2.1" || result.Latency.LoadedAvgMS != 21.123 {
		t.Fatalf("latency = %+v", result.Latency)
	}
	if result.Responsiveness.UploadLoaded.Count != 3 || result.Responsiveness.UploadGrade == "" {
		t.Fatalf("upload-loaded responsiveness not measured: %+v", result.Responsiveness)
	}
	if result.CPUBusyPct != 50.0 {
		t.Fatalf("cpu busy = %.1f, want 50.0", result.CPUBusyPct)
	}
}

func TestRunPingRejectsLeadingDashTarget(t *testing.T) {
	got := RunPing(context.Background(), "-f", 1)
	if !strings.Contains(got.Error, "invalid ping target") {
		t.Fatalf("error = %q, want invalid-target rejection", got.Error)
	}
}

func TestRunSpeedTestPingHonorsOverallBudget(t *testing.T) {
	start := time.Now()
	result := RunSpeedTest(context.Background(), SpeedTestOptions{
		MaxDurationSeconds: 1,
		SkipLoadedLatency:  true,
		EndpointFactory: func(rawURL string) (SpeedEndpoint, error) {
			return &fakeSpeedEndpoint{host: rawURL}, nil
		},
		Ping: func(ctx context.Context, target string, count uint32) LatencyResult {
			<-ctx.Done()
			return LatencyResult{Error: "ping cancelled"}
		},
		CPUReader: func() (CPUSample, bool) { return CPUSample{}, false },
	})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("speed test overran its 1s budget: %v", elapsed)
	}
	if result.Latency.Error != "ping cancelled" {
		t.Fatalf("latency = %+v, want bounded ping error", result.Latency)
	}
	if result.Download.Error != "overall time budget exhausted before download phase" {
		t.Fatalf("download = %+v, want budget-exhausted skip", result.Download)
	}
	if result.Upload.Error != "overall time budget exhausted before upload phase" {
		t.Fatalf("upload = %+v, want budget-exhausted skip", result.Upload)
	}
}

func TestCPUSampleParsingAndBusyDelta(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stat")
	if err := os.WriteFile(path, []byte("cpu  100 0 50 800 50 0 0 0 0 0\n"), 0o644); err != nil {
		t.Fatalf("write stat: %v", err)
	}
	before, ok := readCPUBusyFile(path)
	if !ok {
		t.Fatal("readCPUBusyFile returned !ok")
	}
	if before.Busy != 150 || before.Total != 1000 {
		t.Fatalf("before = %+v, want busy=150 total=1000", before)
	}
	after := CPUSample{Busy: 250, Total: 1200}
	if got := cpuBusyPct(before, after); got != 50.0 {
		t.Fatalf("cpuBusyPct = %.1f, want 50.0", got)
	}
}

func (e *fakeSpeedEndpoint) Host() string { return e.host }

func (e *fakeSpeedEndpoint) Download(ctx context.Context, nbytes uint64, deadline time.Time) (uint64, time.Duration, time.Duration, error) {
	_ = ctx
	_ = deadline
	return nbytes, 3 * time.Millisecond, 100 * time.Millisecond, nil
}

func (e *fakeSpeedEndpoint) StreamDownload(ctx context.Context, nbytes uint64, deadline time.Time, onRead func(int) bool) error {
	_ = ctx
	_ = deadline
	// Honor the offered byte count exactly; SpeedEndpoint implementations may
	// never read or report beyond the caller's hard cap.
	remaining := nbytes
	for remaining > 0 {
		n := min(remaining, uint64(1024*1024))
		if !onRead(int(n)) {
			return nil
		}
		remaining -= n
	}
	return nil
}

func TestHTTPStreamDownloadPhysicallyLimitsBodyReads(t *testing.T) {
	const capBytes = 1024
	bodyReader := &countingReader{reader: bytes.NewReader(make([]byte, 4*capBytes))}
	endpoint := &httpEndpoint{
		u:    &url.URL{Scheme: "https", Host: "speed.example", Path: "/__down"},
		host: "speed.example",
		client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			_ = request
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bodyReader),
				Header:     make(http.Header),
			}, nil
		})},
	}
	callbackBytes := 0
	err := endpoint.StreamDownload(
		context.Background(),
		capBytes,
		time.Now().Add(time.Second),
		func(n int) bool {
			callbackBytes += n
			return true
		},
	)
	if err != nil {
		t.Fatalf("StreamDownload: %v", err)
	}
	if bodyReader.read != capBytes || callbackBytes != capBytes {
		t.Fatalf("body/callback bytes = %d/%d, want hard cap %d", bodyReader.read, callbackBytes, capBytes)
	}
}

func (e *fakeSpeedEndpoint) Upload(ctx context.Context, nbytes uint64, deadline time.Time) (uint64, time.Duration, error) {
	_ = ctx
	_ = deadline
	return nbytes, 100 * time.Millisecond, nil
}

func (e *fakeSpeedEndpoint) Close() error { return nil }
