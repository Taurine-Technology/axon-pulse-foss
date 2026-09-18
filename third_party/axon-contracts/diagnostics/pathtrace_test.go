package diagnostics

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fakeTracePath builds a SendProbe hook simulating a path of the given hop
// addresses; "" entries are silent hops. The final entry answers as the
// target (reached).
func fakeTracePath(hops []string, rtt time.Duration) func(context.Context, int, int, time.Duration) traceProbeOutcome {
	return func(_ context.Context, ttl int, _ int, _ time.Duration) traceProbeOutcome {
		if ttl > len(hops) {
			return traceProbeOutcome{}
		}
		addr := hops[ttl-1]
		if addr == "" {
			return traceProbeOutcome{}
		}
		return traceProbeOutcome{
			Answered: true,
			From:     addr,
			Reached:  ttl == len(hops),
			RTT:      rtt + time.Duration(ttl)*time.Millisecond,
		}
	}
}

func noLookup(context.Context, string) string { return "" }

func TestRunPathTraceReachesTarget(t *testing.T) {
	path := []string{"192.168.1.1", "10.0.0.1", "1.1.1.1"}
	result := RunPathTrace(context.Background(), PathTraceOptions{
		Target:        "1.1.1.1",
		QueriesPerHop: 2,
		SendProbe:     fakeTracePath(path, 5*time.Millisecond),
		ReverseLookup: noLookup,
	})

	if !result.Reached {
		t.Fatalf("expected reached, got %+v", result)
	}
	if len(result.Hops) != 3 {
		t.Fatalf("expected 3 hops, got %d", len(result.Hops))
	}
	if result.Hops[0].Addr != "192.168.1.1" || result.Hops[2].Addr != "1.1.1.1" {
		t.Fatalf("unexpected hop addrs: %+v", result.Hops)
	}
	for _, hop := range result.Hops {
		if len(hop.RTTMS) != 2 {
			t.Fatalf("hop %d: expected 2 RTT samples, got %d", hop.TTL, len(hop.RTTMS))
		}
		if hop.LossPct != 0 {
			t.Fatalf("hop %d: expected 0%% loss, got %v", hop.TTL, hop.LossPct)
		}
	}
	if result.Error != "" {
		t.Fatalf("expected no error, got %q", result.Error)
	}
}

func TestRunPathTraceSilentHopLossAndEarlyStop(t *testing.T) {
	// Hop 2 is silent; hops 3.. never answer either, so the ramp must stop
	// after maxTraceSilentHops consecutive silent rungs.
	path := []string{"192.168.1.1", "", "", "", "", "", "", ""}
	result := RunPathTrace(context.Background(), PathTraceOptions{
		Target:        "203.0.113.9",
		QueriesPerHop: 1,
		SendProbe:     fakeTracePath(path, time.Millisecond),
		ReverseLookup: noLookup,
	})

	if result.Reached {
		t.Fatalf("must not reach: %+v", result)
	}
	wantHops := 1 + maxTraceSilentHops
	if len(result.Hops) != wantHops {
		t.Fatalf("expected early stop at %d hops, got %d", wantHops, len(result.Hops))
	}
	silent := result.Hops[1]
	if silent.Addr != "" || silent.LossPct != 100 {
		t.Fatalf("expected fully-silent hop, got %+v", silent)
	}
	if !strings.Contains(result.Error, "consecutive silent hops") {
		t.Fatalf("expected silent-hop partial error, got %q", result.Error)
	}
}

func TestRunPathTraceBudgetStopsRamp(t *testing.T) {
	slowProbe := func(_ context.Context, ttl int, _ int, timeout time.Duration) traceProbeOutcome {
		time.Sleep(30 * time.Millisecond)
		_ = timeout
		return traceProbeOutcome{Answered: true, From: "10.0.0.1", RTT: time.Millisecond, Reached: false}
	}
	result := RunPathTrace(context.Background(), PathTraceOptions{
		Target:        "203.0.113.9",
		MaxHops:       30,
		QueriesPerHop: 1,
		TotalBudgetMS: 100,
		SendProbe:     slowProbe,
		ReverseLookup: noLookup,
	})

	if result.Reached {
		t.Fatal("must not reach")
	}
	if len(result.Hops) == 0 || len(result.Hops) >= 30 {
		t.Fatalf("expected budget to stop the ramp early, got %d hops", len(result.Hops))
	}
	if !strings.Contains(result.Error, "budget") {
		t.Fatalf("expected budget partial error, got %q", result.Error)
	}
}

func TestRunPathTraceClampsCaps(t *testing.T) {
	probes := 0
	countProbe := func(_ context.Context, _ int, _ int, _ time.Duration) traceProbeOutcome {
		probes++
		return traceProbeOutcome{}
	}
	result := RunPathTrace(context.Background(), PathTraceOptions{
		Target:        "203.0.113.9",
		MaxHops:       200,            // above hard cap
		QueriesPerHop: 50,             // above hard cap
		TotalBudgetMS: 10 * 60 * 1000, // above hard cap
		SendProbe:     countProbe,
		ReverseLookup: noLookup,
	})

	// Every hop is silent, so the silent-run stop fires first — but each
	// probed hop must have used at most the capped query count.
	for _, hop := range result.Hops {
		if got := int(hop.TTL); got > maxTraceHops {
			t.Fatalf("hop ttl %d above cap", got)
		}
	}
	if probes > maxTraceSilentHops*maxTraceQueriesPerHop {
		t.Fatalf("probe fan-out not capped: %d probes", probes)
	}
}

func TestRunPathTraceResolveFailure(t *testing.T) {
	result := RunPathTrace(context.Background(), PathTraceOptions{
		Target:        "definitely-not-a-real-hostname.invalid",
		SendProbe:     fakeTracePath([]string{"1.1.1.1"}, time.Millisecond),
		ReverseLookup: noLookup,
	})
	if result.Error == "" || !strings.Contains(result.Error, "resolve") {
		t.Fatalf("expected resolve error, got %+v", result)
	}
	if len(result.Hops) != 0 {
		t.Fatalf("expected no hops, got %d", len(result.Hops))
	}
}
