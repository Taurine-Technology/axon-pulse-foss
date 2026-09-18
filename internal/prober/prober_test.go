package prober

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Taurine-Technology/axon-contracts/gen/go/diagnostics"
	"github.com/Taurine-Technology/axon-pulse/internal/protocol"
)

func TestLatencyResolvesControllerAndAnchor(t *testing.T) {
	t.Parallel()
	engine := &Engine{Now: func() time.Time { return time.Unix(1_800_000_000, 0) }, Ping: func(_ context.Context, target string, _ uint32) diagnostics.LatencyResult {
		return diagnostics.LatencyResult{Target: target, AvgMS: 12, SamplesMS: []float64{12}, ProbesSent: 1}
	}}
	results := engine.Latency(context.Background(), protocol.SensorConfig{ProbeTargets: []string{"controller", "anchor:1.1.1.1"}}, "https://axon.example")
	if len(results) != 2 || results[0].Target != "controller" || results[1].Target != "anchor:1.1.1.1" || !results[0].Success {
		t.Fatalf("unexpected results: %+v", results)
	}
}

func TestLatencyTCPFallbackAllowsExplicitPrivateGateway(t *testing.T) {
	t.Parallel()
	engine := &Engine{
		Now: func() time.Time { return time.Unix(1_800_000_000, 0) },
		Ping: func(_ context.Context, target string, _ uint32) diagnostics.LatencyResult {
			return diagnostics.LatencyResult{Target: target, Error: "icmp unavailable"}
		},
		Dial: func(_ context.Context, address string) (time.Duration, error) {
			if address != "192.168.1.1:80" {
				t.Fatalf("fallback address = %q", address)
			}
			return 12 * time.Millisecond, nil
		},
	}
	// resolveTargets obtains the real gateway, so exercise the class-sensitive
	// dial path directly with the same private target representation.
	target := pingTarget{label: "gateway", address: "192.168.1.1", tcpAddress: "192.168.1.1:80", class: protocol.TargetGateway}
	result := engine.ping()(context.Background(), target.address, 1)
	if result.Error == "" {
		t.Fatal("test ping unexpectedly succeeded")
	}
	elapsed, err := engine.dial(false)(context.Background(), target.tcpAddress)
	if err != nil || elapsed != 12*time.Millisecond {
		t.Fatalf("private fallback = %v, %v", elapsed, err)
	}
}

func TestLatencyCapsDirectCallersAtEightTargets(t *testing.T) {
	t.Parallel()
	targets := make([]string, 20)
	for index := range targets {
		targets[index] = fmt.Sprintf("192.0.2.%d", index+1)
	}
	engine := &Engine{Ping: func(_ context.Context, target string, _ uint32) diagnostics.LatencyResult {
		return diagnostics.LatencyResult{Target: target, SamplesMS: []float64{1}}
	}}
	results := engine.Latency(context.Background(), protocol.SensorConfig{ProbeTargets: targets}, "https://axon.example")
	if len(results) != maxLatencyTargets {
		t.Fatalf("Latency returned %d targets, want %d", len(results), maxLatencyTargets)
	}
}

func TestParseLinuxDefaultRoute(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux route format")
	}
	input := "Iface Destination Gateway Flags RefCnt Use Metric Mask MTU Window IRTT\neth0 00000000 0102A8C0 0003 0 0 100 00000000 0 0 0\n"
	got, err := parseRoute(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if got != "192.168.2.1" {
		t.Fatalf("gateway = %s", got)
	}
}
