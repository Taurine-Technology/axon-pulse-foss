package protocol

import (
	"net"
	"testing"
)

func TestIsPublicIPRejectsLocalUseNAT64(t *testing.T) {
	t.Parallel()
	if IsPublicIP(net.ParseIP("64:ff9b:1::1")) {
		t.Fatal("local-use NAT64 address was accepted")
	}
	if !IsPublicIP(net.ParseIP("2606:4700:4700::1111")) {
		t.Fatal("public IPv6 address was rejected")
	}
}

func TestDefaultConfigCoversIndependentAnchorsAndWebTargets(t *testing.T) {
	t.Parallel()
	config := DefaultConfig()
	wantProbes := []string{"gateway", "controller", "anchor:1.1.1.1", "anchor:8.8.8.8"}
	if len(config.ProbeTargets) != len(wantProbes) {
		t.Fatalf("probe targets = %#v", config.ProbeTargets)
	}
	for index := range wantProbes {
		if config.ProbeTargets[index] != wantProbes[index] {
			t.Fatalf("probe targets = %#v", config.ProbeTargets)
		}
	}
	if len(config.DNSResolvers) != 1 || config.DNSResolvers[0] != "1.1.1.1" {
		t.Fatalf("DNS resolvers = %#v", config.DNSResolvers)
	}
	if len(config.HTTPTargets) != 2 {
		t.Fatalf("HTTP targets = %#v", config.HTTPTargets)
	}
}

func TestNormalizeConfigRestoresUnsafeEmptyDefaults(t *testing.T) {
	t.Parallel()
	config := NormalizeConfig(SensorConfig{})
	if len(config.ProbeTargets) != 4 || len(config.DNSResolvers) != 1 || len(config.HTTPTargets) != 2 {
		t.Fatalf("normalized defaults = %#v", config)
	}
}

func TestNormalizeConfigRejectsUnsafeTargetsAndClampsProfiles(t *testing.T) {
	t.Parallel()
	config := NormalizeConfig(SensorConfig{
		ProbeTargets: []string{"file:///etc/passwd"}, HTTPTargets: []string{"http://insecure.example"},
		SpeedProfiles: []SpeedProfileConfig{{Name: "capacity", Enabled: true, MinDurationSeconds: 1, MaxDurationSeconds: 900, MaxDownloadBytes: 9 << 40, MaxUploadBytes: 9 << 40, MaxConcurrentRequests: 99, MinSpacingMinutes: 1, Windows: []string{"00:00-24:00"}}},
	})
	if config.ProbeTargets[0] != "gateway" || config.HTTPTargets[0] != DefaultConfig().HTTPTargets[0] {
		t.Fatalf("unsafe targets survived: %+v", config)
	}
	profile := config.SpeedProfiles[0]
	if profile.Name != "capacity" || profile.MaxDurationSeconds > 120 || profile.MaxConcurrentRequests > 8 || profile.MinSpacingMinutes < 15 || profile.MaxDownloadBytes > 1<<30 {
		t.Fatalf("profile escaped local clamps: %+v", profile)
	}
}

func TestNormalizeConfigDropsMalformedWindowsAndRestoresFallback(t *testing.T) {
	t.Parallel()
	config := NormalizeConfig(SensorConfig{SpeedProfiles: []SpeedProfileConfig{{
		Name: "content", Enabled: true, Windows: []string{"banana", "25:00-26:00", "10:00-10:00"},
		MinDurationSeconds: 4, MaxDurationSeconds: 20, MaxDownloadBytes: 7 << 20,
		MaxUploadBytes: 3 << 20, MaxConcurrentRequests: 4, MinSpacingMinutes: 15,
	}}})
	content := config.SpeedProfiles[0]
	if content.Name != "content" || len(content.Windows) == 0 {
		t.Fatalf("malformed windows left profile unschedulable: %+v", content)
	}
	for _, window := range content.Windows {
		if !validSpeedWindow(window) {
			t.Fatalf("invalid window %q survived normalization", window)
		}
	}
	mixed := NormalizeConfig(SensorConfig{SpeedProfiles: []SpeedProfileConfig{{
		Name: "content", Enabled: true, Windows: []string{"garbage", "06:00-22:00"},
		MinDurationSeconds: 4, MaxDurationSeconds: 20, MaxDownloadBytes: 7 << 20,
		MaxUploadBytes: 3 << 20, MaxConcurrentRequests: 4, MinSpacingMinutes: 15,
	}}})
	if got := mixed.SpeedProfiles[0].Windows; len(got) != 1 || got[0] != "06:00-22:00" {
		t.Fatalf("mixed windows = %v, want just the valid entry", got)
	}
	never := NormalizeConfig(SensorConfig{SpeedProfiles: []SpeedProfileConfig{{
		Name: "capacity", Enabled: false, Windows: []string{"never"},
		MinDurationSeconds: 4, MaxDurationSeconds: 20, MaxDownloadBytes: 7 << 20,
		MaxUploadBytes: 3 << 20, MaxConcurrentRequests: 4, MinSpacingMinutes: 15,
	}}})
	for _, profile := range never.SpeedProfiles {
		if profile.Name == "capacity" && (len(profile.Windows) != 1 || profile.Windows[0] != "never") {
			t.Fatalf("never sentinel did not survive: %+v", profile)
		}
	}
}

func TestNormalizeConfigClampsCustomCadence(t *testing.T) {
	t.Parallel()
	config := NormalizeConfig(SensorConfig{SpeedProfiles: []SpeedProfileConfig{{
		Name: "content", Enabled: true, Windows: []string{"06:00-22:00"}, CadenceMinutes: 5,
		MinDurationSeconds: 4, MaxDurationSeconds: 20, MaxDownloadBytes: 7 << 20,
		MaxUploadBytes: 3 << 20, MaxConcurrentRequests: 4, MinSpacingMinutes: 15,
	}}})
	content, ok := func() (SpeedProfileConfig, bool) {
		for _, profile := range config.SpeedProfiles {
			if profile.Name == "content" {
				return profile, true
			}
		}
		return SpeedProfileConfig{}, false
	}()
	if !ok || content.CadenceMinutes != DefaultConfig().SpeedProfiles[0].CadenceMinutes {
		t.Fatalf("cadence clamp = %+v", content)
	}
}

func TestNormalizeConfigRejectsPrivateTargetsResolversAndRemoteSafetyOptIns(t *testing.T) {
	t.Parallel()
	config := NormalizeConfig(SensorConfig{
		ProbeTargets: []string{"anchor:127.0.0.1"},
		DNSResolvers: []string{"169.254.169.254"},
		HTTPTargets:  []string{"https://10.0.0.1/private"},
		SpeedProfiles: []SpeedProfileConfig{{
			Name: "content", Enabled: true, Windows: []string{"00:00-24:00"}, MinDurationSeconds: 4, MaxDurationSeconds: 20,
			MaxDownloadBytes: 7 << 20, MaxUploadBytes: 3 << 20, MaxConcurrentRequests: 4, MinSpacingMinutes: 15,
			AllowMetered: true, AllowOnBattery: true,
		}},
	})
	defaults := DefaultConfig()
	if config.ProbeTargets[0] != defaults.ProbeTargets[0] || config.DNSResolvers[0] != defaults.DNSResolvers[0] || config.HTTPTargets[0] != defaults.HTTPTargets[0] {
		t.Fatalf("private controller targets survived normalization: %+v", config)
	}
	content := config.SpeedProfiles[0]
	if content.AllowMetered || content.AllowOnBattery {
		t.Fatalf("remote config relaxed local safety gates: %+v", content)
	}
	if len(config.SpeedProfiles) != 2 {
		t.Fatalf("missing safe default profile: %+v", config.SpeedProfiles)
	}
}
