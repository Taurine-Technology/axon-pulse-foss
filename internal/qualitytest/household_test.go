package qualitytest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Taurine-Technology/axon-pulse/internal/protocol"
	"github.com/Taurine-Technology/axon-pulse/quality"
)

// householdServer serves the download/upload shape the household engine
// expects and records how many distinct connections carried bulk work.
func householdServer(t *testing.T) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	connections := map[string]bool{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Server-Timing", "app;dur=0.1")
		switch request.URL.Path {
		case "/down":
			amount, _ := strconv.ParseInt(request.URL.Query().Get("bytes"), 10, 64)
			if amount > 0 {
				mu.Lock()
				connections[request.RemoteAddr] = true
				mu.Unlock()
				_, _ = io.CopyN(writer, zeroReader{}, amount)
			}
		case "/up":
			mu.Lock()
			connections[request.RemoteAddr] = true
			mu.Unlock()
			_, _ = io.Copy(io.Discard, request.Body)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	return server, func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(connections)
	}
}

// shortenHouseholdWindow makes the engine's fixed window short enough for a
// unit test while keeping every activity above its minimum sample count.
// Tests using it must not run in parallel.
func shortenHouseholdWindow(t *testing.T) {
	t.Helper()
	warmup, observe, minimum := quality.HouseholdWarmupMS, quality.HouseholdObserveMS, quality.HouseholdMinObserveMS
	quality.HouseholdWarmupMS, quality.HouseholdObserveMS, quality.HouseholdMinObserveMS = 500, 7000, 2000
	t.Cleanup(func() {
		quality.HouseholdWarmupMS, quality.HouseholdObserveMS, quality.HouseholdMinObserveMS = warmup, observe, minimum
	})
}

func TestHouseholdRunPacesEveryActivityAndScoresTheMix(t *testing.T) {
	shortenHouseholdWindow(t)
	server, connections := householdServer(t)
	profile := protocol.SpeedProfileConfig{
		Name: protocol.ProfileHousehold, Enabled: true, MinDurationSeconds: 8, MaxDurationSeconds: 12,
		MaxDownloadBytes: 225_000_000, MaxUploadBytes: 75_000_000, MaxConcurrentRequests: 8, CountsTowardExperience: true,
	}
	mix := quality.HouseholdMix{HDStreams: 1, VideoCalls: 1, Gaming: 1, Browsing: true}
	var progressMu sync.Mutex
	phases := map[string]bool{}
	result, err := (Runner{}).Run(context.Background(), Options{
		Profile: profile, DownloadURL: server.URL + "/down", UploadURL: server.URL + "/up", Client: server.Client(),
		HouseholdMix: mix, MixSource: quality.MixSourceLocal,
		OnProgress: func(p protocol.SpeedTestProgress) {
			progressMu.Lock()
			phases[p.Phase] = true
			progressMu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The run reports its own duration; the wall clock on a loaded CI host
	// is not evidence of anything.
	if result.DurationMS <= 0 || result.DurationMS > float64(profile.MaxDurationSeconds*1000+2000) {
		t.Fatalf("reported duration %.0f ms exceeds the profile maximum", result.DurationMS)
	}
	household := result.Household
	if household == nil {
		t.Fatal("household block missing")
	}
	if result.Profile != protocol.ProfileHousehold || household.Mix != mix || household.MixSource != quality.MixSourceLocal || household.MixRevision != mix.Revision() {
		t.Fatalf("identity = %+v", household)
	}
	if household.StopReason != quality.StopEnoughEvidence || household.Status != quality.HouseholdStatusPass {
		t.Fatalf("status=%s stop=%s summary=%q activities=%+v", household.Status, household.StopReason, household.Summary, household.Activities)
	}
	// The reference does not fit a 12 s test, so the plan drops it rather
	// than shrinking the mix or its window.
	if household.Reference != nil {
		t.Fatalf("reference should have been dropped: %+v", household.Reference)
	}
	if household.Window.ObservedMS < quality.HouseholdObserveMS-500 || household.Window.WarmupMS != quality.HouseholdWarmupMS {
		t.Fatalf("window = %+v", household.Window)
	}
	if len(household.Activities) != 4 {
		t.Fatalf("activities = %+v", household.Activities)
	}
	seen := map[string]quality.HouseholdActivity{}
	for _, activity := range household.Activities {
		seen[activity.Key] = activity
		if !activity.Available || activity.Rating != quality.RatingGood {
			t.Fatalf("activity %s not scored good on a loopback server: %+v", activity.Key, activity)
		}
	}
	stream := seen["stream_hd"]
	if stream.AchievedDownMbps < stream.DemandDownMbps*0.9 || stream.LateSamples != 0 {
		t.Fatalf("stream = %+v", stream)
	}
	call := seen["video_call"]
	if call.AchievedUpMbps < call.DemandUpMbps*0.85 || call.AchievedDownMbps < call.DemandDownMbps*0.85 {
		t.Fatalf("call did not reach its paced demand: %+v", call)
	}
	if !seen["game"].Estimate || seen["game"].Samples < 100 {
		t.Fatalf("game = %+v", seen["game"])
	}
	if seen["browsing"].ColdP95MS <= 0 {
		t.Fatalf("browsing never measured a cold connection: %+v", seen["browsing"])
	}
	ledger := household.Ledger
	if ledger.UsedBytes == 0 || ledger.UsedBytes != ledger.DownloadBytes+ledger.UploadBytes+ledger.ProbeBytes || ledger.UsedBytes > ledger.CapBytes {
		t.Fatalf("ledger = %+v", ledger)
	}
	if result.Budget.UsedBytes != ledger.UsedBytes || result.Budget.CapHit {
		t.Fatalf("budget = %+v", result.Budget)
	}
	// Paced demand of the mix is about 7.7 Mbps; the aggregate must reflect
	// pacing, not the loopback link's capacity.
	if result.Download.ThroughputMbps > 20 || result.Upload.ThroughputMbps > 10 {
		t.Fatalf("aggregate throughput looks saturated, not paced: down=%.1f up=%.1f", result.Download.ThroughputMbps, result.Upload.ThroughputMbps)
	}
	if !result.Experience.Available || result.Experience.Home["streaming"].Rating != quality.RatingGood || result.Experience.Home["video_calls"].Rating != quality.RatingGood {
		t.Fatalf("experience = %+v", result.Experience)
	}
	if result.Responsiveness.BidirectionalLoaded == nil || result.Responsiveness.BidirectionalLoaded.Count == 0 || result.Responsiveness.OverallGrade == "" {
		t.Fatalf("latency under household load missing: %+v", result.Responsiveness)
	}
	for _, phase := range []string{"baseline", "household", "cooldown"} {
		evidence, ok := result.Phases[phase]
		if !ok || evidence.EndMS < evidence.StartMS {
			t.Fatalf("phase %s evidence = %+v", phase, evidence)
		}
	}
	if len(result.Phases["household"].Buckets) == 0 || result.Phases["household"].StableRegion.Reason != "paced_scenario" {
		t.Fatalf("household phase evidence = %+v", result.Phases["household"])
	}
	progressMu.Lock()
	defer progressMu.Unlock()
	for _, phase := range []string{"baseline", "household", "cooldown", "done"} {
		if !phases[phase] {
			t.Fatalf("progress never reported %q: %v", phase, phases)
		}
	}
	if connections() < 3 {
		t.Fatalf("expected separate HTTP/1.1 connections per activity, saw %d", connections())
	}
}

func TestHouseholdRunStopsAtTheByteCapAndSaysSo(t *testing.T) {
	shortenHouseholdWindow(t)
	server, _ := householdServer(t)
	// The smallest permitted envelope cannot fund even one 4K segment, so
	// the ledger must refuse the unit rather than let the stream run below
	// its demand, and the result must name the budget, not the network.
	profile := protocol.SpeedProfileConfig{
		Name: protocol.ProfileHousehold, Enabled: true, MinDurationSeconds: 8, MaxDurationSeconds: 12,
		MaxDownloadBytes: 512 << 10, MaxUploadBytes: 256 << 10, MaxConcurrentRequests: 8, CountsTowardExperience: true,
	}
	result, err := (Runner{}).Run(context.Background(), Options{
		Profile: profile, DownloadURL: server.URL + "/down", UploadURL: server.URL + "/up", Client: server.Client(),
		HouseholdMix: quality.HouseholdMix{UHDStreams: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	household := result.Household
	if household == nil || household.StopReason != quality.StopInsufficientBytes || household.Status != quality.HouseholdStatusInsufficient {
		t.Fatalf("household = %+v", household)
	}
	if !result.Budget.CapHit || household.Ledger.UsedBytes > household.Ledger.CapBytes {
		t.Fatalf("budget = %+v ledger = %+v", result.Budget, household.Ledger)
	}
	if result.Experience.Available || result.MeasurementConfidence.Level == "high" {
		t.Fatalf("a starved run must not look like a verdict: %+v %+v", result.Experience, result.MeasurementConfidence)
	}
	if stream := household.Activities[0]; stream.Available || stream.Rating != quality.RatingUnavailable {
		t.Fatalf("stream = %+v", stream)
	}
}

func TestHouseholdRunHonoursCancellation(t *testing.T) {
	shortenHouseholdWindow(t)
	server, _ := householdServer(t)
	profile, _ := protocol.DeviceSpeedProfile(protocol.ProfileHousehold)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(3 * time.Second)
		cancel()
	}()
	result, err := (Runner{}).Run(ctx, Options{
		Profile: profile, DownloadURL: server.URL + "/down", UploadURL: server.URL + "/up", Client: server.Client(),
		HouseholdMix: quality.DefaultHouseholdMix(),
	})
	if err == nil {
		t.Fatal("cancelled run returned no error")
	}
	if result.Household == nil || result.Household.StopReason != quality.StopCancelled {
		t.Fatalf("stop reason = %+v", result.Household)
	}
}

func TestBidirectionalReserveFollowsTheWindowShare(t *testing.T) {
	t.Parallel()
	// 60 s saturation profile: 18 s one-way phases, 15 s bidirectional.
	phase, window := 18*time.Second, 15*time.Second
	reserve := bidirectionalReserve(330_000_000, phase, window)
	if reserve < 149_000_000 || reserve > 151_000_000 {
		t.Fatalf("reserve = %d, want ~15/33 of the allocation", reserve)
	}
	// A short window never drops below a quarter of the allocation.
	if got := bidirectionalReserve(100, 90*time.Second, 2*time.Second); got != 25 {
		t.Fatalf("floor reserve = %d, want 25", got)
	}
}

func TestSaturationProfileIsAccepted(t *testing.T) {
	t.Parallel()
	for _, name := range []string{protocol.ProfileSaturation, protocol.ProfileCapacity, protocol.ProfileContent} {
		profile := protocol.SpeedProfileConfig{Name: name, MinDurationSeconds: 2, MaxDurationSeconds: 8, MaxDownloadBytes: 1 << 20, MaxUploadBytes: 512 << 10, MaxConcurrentRequests: 2}
		_, err := (Runner{}).Run(context.Background(), Options{Profile: profile, DownloadURL: "https://127.0.0.1:1/down", UploadURL: "https://127.0.0.1:1/up"})
		if err == nil || err.Error() == "quality test profile must be household, saturation, content or capacity" {
			t.Fatalf("profile %s rejected by name: %v", name, err)
		}
	}
	_, err := (Runner{}).Run(context.Background(), Options{Profile: protocol.SpeedProfileConfig{Name: "bogus", MinDurationSeconds: 2, MaxDurationSeconds: 8, MaxDownloadBytes: 1 << 20, MaxUploadBytes: 512 << 10}})
	if err == nil {
		t.Fatal("unknown profile accepted")
	}
}
