package protocol

import (
	"testing"

	"github.com/Taurine-Technology/axon-pulse/quality"
)

func profileNames(profiles []SpeedProfileConfig) []string {
	names := make([]string, 0, len(profiles))
	for _, profile := range profiles {
		names = append(names, profile.Name)
	}
	return names
}

func TestDefaultConfigSchedulesHouseholdAndSaturation(t *testing.T) {
	t.Parallel()
	config := DefaultConfig()
	if config.ContractVersion != HouseholdContractVersion {
		t.Fatalf("contract version = %d", config.ContractVersion)
	}
	if got := profileNames(config.SpeedProfiles); len(got) != 2 || got[0] != ProfileHousehold || got[1] != ProfileSaturation {
		t.Fatalf("default profiles = %v", got)
	}
	household := config.SpeedProfiles[0]
	if !household.Enabled || household.CadenceMinutes != 360 || !household.CountsTowardExperience || household.MaxDownloadBytes+household.MaxUploadBytes != 300_000_000 {
		t.Fatalf("household profile = %+v", household)
	}
	saturation := config.SpeedProfiles[1]
	if !saturation.Enabled || saturation.CadenceMinutes != 1440 || saturation.CountsTowardExperience || saturation.MaxDownloadBytes != 1<<30 {
		t.Fatalf("saturation profile = %+v", saturation)
	}
	// Four household tests and a daily saturation test on a fast link must
	// fit the ledgers the sensor enforces.
	if config.DailyDataBudgetMB < 4096 || config.MonthlyDataBudgetMB < 61440 {
		t.Fatalf("budgets daily=%d monthly=%d", config.DailyDataBudgetMB, config.MonthlyDataBudgetMB)
	}
	if NormalizeConfig(config).SpeedProfiles[0].Name != ProfileHousehold {
		t.Fatal("defaults do not survive normalization")
	}
}

func TestContractTwoControllersNeverReceiveHouseholdProfiles(t *testing.T) {
	t.Parallel()
	// An older controller only knows the legacy pair. Even if it were sent
	// household profiles, the sensor must not schedule them: the controller
	// would reject the household block on ingest.
	legacy := NormalizeConfig(SensorConfig{ContractVersion: 2, SpeedProfiles: []SpeedProfileConfig{
		{Name: ProfileHousehold, Enabled: true, Windows: []string{"00:00-24:00"}},
		{Name: ProfileContent, Enabled: true, Windows: []string{"00:00-24:00"}, MinDurationSeconds: 4, MaxDurationSeconds: 20, MaxDownloadBytes: 7 << 20, MaxUploadBytes: 3 << 20, MaxConcurrentRequests: 4, MinSpacingMinutes: 15},
	}, HouseholdMix: &quality.HouseholdMix{HDStreams: 1}})
	if got := profileNames(legacy.SpeedProfiles); len(got) != 2 || got[0] != ProfileContent || got[1] != ProfileCapacity {
		t.Fatalf("contract-2 profiles = %v", got)
	}
	if legacy.HouseholdMix != nil {
		t.Fatal("contract-2 config kept a household mix")
	}
	if !legacy.SpeedProfiles[0].CountsTowardExperience || legacy.SpeedProfiles[1].CountsTowardExperience {
		t.Fatalf("experience flags = %+v", legacy.SpeedProfiles)
	}
	// A config with no contract version is treated as contract 2.
	if got := profileNames(NormalizeConfig(SensorConfig{}).SpeedProfiles); got[0] != ProfileContent {
		t.Fatalf("unversioned profiles = %v", got)
	}
}

func TestContractThreeControllersGetHouseholdDefaultsAndBoundedMix(t *testing.T) {
	t.Parallel()
	config := NormalizeConfig(SensorConfig{ContractVersion: 3, SpeedProfiles: []SpeedProfileConfig{
		{Name: ProfileSaturation, Enabled: false, Windows: []string{"never"}, MinDurationSeconds: 8, MaxDurationSeconds: 60, MaxDownloadBytes: 1 << 30, MaxUploadBytes: 512 << 20, MaxConcurrentRequests: 8, MinSpacingMinutes: 360},
		{Name: ProfileContent, Enabled: true, Windows: []string{"00:00-24:00"}, MinDurationSeconds: 4, MaxDurationSeconds: 20, MaxDownloadBytes: 7 << 20, MaxUploadBytes: 3 << 20, MaxConcurrentRequests: 4, MinSpacingMinutes: 15},
	}, HouseholdMix: &quality.HouseholdMix{UHDStreams: 9, VideoCalls: 1}})
	if got := profileNames(config.SpeedProfiles); len(got) != 3 || got[0] != ProfileSaturation || got[1] != ProfileContent || got[2] != ProfileHousehold {
		t.Fatalf("contract-3 profiles = %v", got)
	}
	if config.SpeedProfiles[0].Enabled || config.SpeedProfiles[0].CountsTowardExperience {
		t.Fatalf("saturation flags = %+v", config.SpeedProfiles[0])
	}
	if !config.SpeedProfiles[2].Enabled || !config.SpeedProfiles[2].CountsTowardExperience {
		t.Fatalf("appended household default = %+v", config.SpeedProfiles[2])
	}
	if config.HouseholdMix == nil || *config.HouseholdMix != (quality.HouseholdMix{UHDStreams: 4, VideoCalls: 1}) {
		t.Fatalf("mix = %+v", config.HouseholdMix)
	}
	// More than four profiles are truncated; unknown names are dropped.
	many := NormalizeConfig(SensorConfig{ContractVersion: 3, SpeedProfiles: []SpeedProfileConfig{
		{Name: "bogus"}, {Name: ProfileHousehold, Windows: []string{"00:00-24:00"}}, {Name: ProfileSaturation, Windows: []string{"never"}}, {Name: ProfileContent, Windows: []string{"never"}}, {Name: ProfileCapacity, Windows: []string{"never"}},
	}})
	if got := profileNames(many.SpeedProfiles); len(got) != 3 || got[0] != ProfileHousehold {
		t.Fatalf("truncated profiles = %v", got)
	}
}

func TestEffectiveHouseholdMixPrecedence(t *testing.T) {
	t.Parallel()
	local := &quality.HouseholdMix{HDStreams: 1}
	controller := &quality.HouseholdMix{FHDStreams: 2, Gaming: 1}
	if mix, source := EffectiveHouseholdMix(local, controller); mix != *local || source != quality.MixSourceLocal {
		t.Fatalf("local override = %+v %s", mix, source)
	}
	if mix, source := EffectiveHouseholdMix(nil, controller); mix != *controller || source != quality.MixSourceController {
		t.Fatalf("controller mix = %+v %s", mix, source)
	}
	if mix, source := EffectiveHouseholdMix(nil, nil); mix != quality.DefaultHouseholdMix() || source != quality.MixSourceDefault {
		t.Fatalf("default mix = %+v %s", mix, source)
	}
}

func TestDeviceSpeedProfileCoversLegacyNames(t *testing.T) {
	t.Parallel()
	for _, name := range []string{ProfileHousehold, ProfileSaturation, ProfileContent, ProfileCapacity} {
		if profile, ok := DeviceSpeedProfile(name); !ok || profile.Name != name || !KnownSpeedProfile(name) {
			t.Fatalf("device profile %s = %+v %v", name, profile, ok)
		}
	}
	if _, ok := DeviceSpeedProfile("bogus"); ok || KnownSpeedProfile("bogus") {
		t.Fatal("bogus profile accepted")
	}
	if !SaturationProfile(ProfileSaturation) || !SaturationProfile(ProfileCapacity) || SaturationProfile(ProfileHousehold) {
		t.Fatal("saturation classification wrong")
	}
	if !ExperienceProfile(ProfileHousehold) || !ExperienceProfile(ProfileContent) || ExperienceProfile(ProfileSaturation) {
		t.Fatal("experience classification wrong")
	}
}

func TestHouseholdProfileNormalizationKeepsTheEngineDurationFloor(t *testing.T) {
	t.Parallel()
	config := NormalizeConfig(SensorConfig{ContractVersion: 3, SpeedProfiles: []SpeedProfileConfig{
		{Name: ProfileHousehold, Enabled: true, Windows: []string{"00:00-24:00"}, MinDurationSeconds: 2, MaxDurationSeconds: 4, MaxDownloadBytes: 1 << 20, MaxUploadBytes: 512 << 10, MaxConcurrentRequests: 4, MinSpacingMinutes: 15},
	}})
	household := config.SpeedProfiles[0]
	if household.Name != ProfileHousehold || household.MaxDurationSeconds < 8 || household.MinDurationSeconds > household.MaxDurationSeconds {
		t.Fatalf("household duration bounds = %+v", household)
	}
}
