package service

import (
	"testing"
	"time"

	"github.com/Taurine-Technology/axon-pulse/internal/protocol"
	"github.com/Taurine-Technology/axon-pulse/quality"
)

func TestDefaultSpeedProfileFollowsTheEffectiveConfiguration(t *testing.T) {
	t.Parallel()
	if got := defaultSpeedProfile(protocol.DefaultConfig()); got != protocol.ProfileHousehold {
		t.Fatalf("standalone default = %s", got)
	}
	legacy := protocol.NormalizeConfig(protocol.SensorConfig{ContractVersion: 2})
	if got := defaultSpeedProfile(legacy); got != protocol.ProfileContent {
		t.Fatalf("contract-2 default = %s", got)
	}
}

func TestHouseholdEligibilityRequiresObservedEvidence(t *testing.T) {
	t.Parallel()
	profile, _ := protocol.DeviceSpeedProfile(protocol.ProfileHousehold)
	base := protocol.SpeedTest{
		Profile: protocol.ProfileHousehold,
		Household: &quality.HouseholdResult{
			Status: quality.HouseholdStatusFail, StopReason: quality.StopEnoughEvidence,
			Window: quality.HouseholdWindow{ObservedMS: quality.HouseholdObserveMS},
		},
		Experience:            quality.Experience{Available: true, Rating: quality.RatingBad},
		MeasurementConfidence: quality.Confidence{Level: "high"},
	}
	base.Responsiveness.Baseline.Count = 20
	if eligible, reason := experienceEligibility(base, protocol.TrafficContext{}, profile); !eligible || reason != "" {
		t.Fatalf("failing but well-measured household run excluded: %s", reason)
	}
	cases := map[string]func(*protocol.SpeedTest){
		"household_insufficient_bytes": func(result *protocol.SpeedTest) { result.Household.StopReason = quality.StopInsufficientBytes },
		"household_insufficient": func(result *protocol.SpeedTest) {
			result.Household.Status = quality.HouseholdStatusInsufficient
		},
		"household_evidence_missing":   func(result *protocol.SpeedTest) { result.Household = nil },
		"low_measurement_confidence":   func(result *protocol.SpeedTest) { result.MeasurementConfidence.Level = "low" },
		"latency_evidence_unavailable": func(result *protocol.SpeedTest) { result.Responsiveness.Baseline.Count = 0 },
		"cross_traffic":                func(*protocol.SpeedTest) {},
	}
	for want, mutate := range cases {
		result := base
		household := *base.Household
		result.Household = &household
		mutate(&result)
		traffic := protocol.TrafficContext{Contaminated: want == "cross_traffic"}
		if eligible, reason := experienceEligibility(result, traffic, profile); eligible || reason != want {
			t.Fatalf("case %s: eligible=%v reason=%s", want, eligible, reason)
		}
	}
	saturation, _ := protocol.DeviceSpeedProfile(protocol.ProfileSaturation)
	if eligible, reason := experienceEligibility(base, protocol.TrafficContext{}, saturation); eligible || reason != "profile_excluded" {
		t.Fatalf("saturation result entered experience history: %v %s", eligible, reason)
	}
}

func TestControllerSpeedRequestsAcceptEveryKnownProfile(t *testing.T) {
	t.Parallel()
	for _, name := range []string{protocol.ProfileHousehold, protocol.ProfileSaturation, protocol.ProfileContent, protocol.ProfileCapacity} {
		request := protocol.SpeedTestRequest{Nonce: "nonce_1234567890", Profile: name, IssuedAt: 1_800_000_000, ExpiresAt: 1_800_000_120}
		if reason := validateControllerSpeedRequest(request, time.Unix(request.IssuedAt, 0).Add(time.Minute)); reason == "invalid_profile" {
			t.Fatalf("profile %s rejected", name)
		}
	}
	request := protocol.SpeedTestRequest{Nonce: "nonce_1234567890", Profile: "bogus", IssuedAt: 1_800_000_000, ExpiresAt: 1_800_000_120}
	if reason := validateControllerSpeedRequest(request, time.Unix(request.IssuedAt, 0).Add(time.Minute)); reason != "invalid_profile" {
		t.Fatalf("bogus profile reason = %s", reason)
	}
}
