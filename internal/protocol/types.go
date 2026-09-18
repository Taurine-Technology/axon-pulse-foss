// Package protocol implements the public Axon Pulse sensor protocol.
package protocol

import (
	"encoding/json"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/Taurine-Technology/axon-pulse/quality"
)

type (
	EnrollmentRequest struct {
		Token      string `json:"token"`
		SensorUID  string `json:"sensor_uid"`
		Hostname   string `json:"hostname"`
		OS         string `json:"os"`
		Arch       string `json:"arch"`
		AppVersion string `json:"app_version"`
	}

	EnrollmentResponse struct {
		SensorID      string       `json:"sensor_id"`
		SensorSecret  string       `json:"sensor_secret"`
		SiteID        string       `json:"site_id"`
		Config        SensorConfig `json:"config"`
		ConfigVersion int          `json:"config_version"`
		IngestURL     string       `json:"ingest_url"`
	}

	SensorConfig struct {
		ContractVersion            int                  `json:"contract_version,omitempty"`
		ProbeIntervalSeconds       int                  `json:"probe_interval_seconds"`
		UploadIntervalSeconds      int                  `json:"upload_interval_seconds"`
		DNSIntervalSeconds         int                  `json:"dns_interval_seconds"`
		HTTPIntervalSeconds        int                  `json:"http_interval_seconds"`
		HeartbeatIntervalSeconds   int                  `json:"heartbeat_interval_seconds"`
		ProbeTargets               []string             `json:"probe_targets"`
		DNSDomains                 []string             `json:"dns_domains"`
		DNSResolvers               []string             `json:"dns_resolvers"`
		HTTPTargets                []string             `json:"http_targets"`
		DailyDataBudgetMB          int                  `json:"daily_data_budget_mb"`
		MonthlyDataBudgetMB        int                  `json:"monthly_data_budget_mb,omitempty"`
		SpeedTestWindows           []string             `json:"speed_test_windows"`
		SpeedTestCrossTrafficMbps  float64              `json:"speed_test_cross_traffic_mbps"`
		SpeedTestMinSpacingMinutes int                  `json:"speed_test_min_spacing_minutes"`
		SendSSID                   bool                 `json:"send_ssid"`
		SendNetworkIdentity        bool                 `json:"send_network_identity,omitempty"`
		Paused                     bool                 `json:"paused"`
		SpeedProfiles              []SpeedProfileConfig `json:"speed_profiles,omitempty"`
		// HouseholdMix is the activity mix the scheduled household profile
		// reproduces. Nil means the catalog default.
		HouseholdMix *quality.HouseholdMix `json:"household_mix,omitempty"`
	}

	SpeedProfileConfig struct {
		Name                   string   `json:"name"`
		Enabled                bool     `json:"enabled"`
		Windows                []string `json:"windows"`
		CadenceMinutes         int      `json:"cadence_minutes,omitempty"`
		MinDurationSeconds     int      `json:"min_duration_seconds"`
		MaxDurationSeconds     int      `json:"max_duration_seconds"`
		MaxDownloadBytes       uint64   `json:"max_download_bytes"`
		MaxUploadBytes         uint64   `json:"max_upload_bytes"`
		MaxConcurrentRequests  int      `json:"max_concurrent_requests"`
		MinSpacingMinutes      int      `json:"min_spacing_minutes"`
		AllowMetered           bool     `json:"allow_metered"`
		AllowOnBattery         bool     `json:"allow_on_battery"`
		CountsTowardExperience bool     `json:"counts_toward_experience"`
	}

	RTTSummary struct {
		P50 float64 `json:"p50"`
		P95 float64 `json:"p95"`
		Min float64 `json:"min"`
		Max float64 `json:"max"`
	}

	TargetClass string

	ProbeMethod string

	MinuteMetric struct {
		Timestamp   int64       `json:"ts"`
		Target      string      `json:"target"`
		RTT         RTTSummary  `json:"rtt_ms"`
		JitterMS    float64     `json:"jitter_ms"`
		LossPct     float64     `json:"loss_pct"`
		Samples     int         `json:"samples"`
		Fallback    string      `json:"fallback,omitempty"`
		TargetClass TargetClass `json:"target_class,omitempty"`
		ProbeMethod ProbeMethod `json:"probe_method,omitempty"`
	}

	DNSCheck struct {
		Timestamp int64   `json:"ts"`
		Resolver  string  `json:"resolver"`
		Domain    string  `json:"domain,omitempty"`
		ColdMS    float64 `json:"cold_ms"`
		WarmMS    float64 `json:"warm_ms"`
		Status    string  `json:"status,omitempty"`
		Hijack    bool    `json:"hijack"`
		Error     string  `json:"error,omitempty"`
	}

	HTTPCheck struct {
		Timestamp   int64       `json:"ts"`
		Target      string      `json:"target"`
		DNSMS       float64     `json:"dns_ms"`
		ConnectMS   float64     `json:"connect_ms"`
		TLSMS       float64     `json:"tls_ms"`
		TTFBMS      float64     `json:"ttfb_ms"`
		TotalMS     float64     `json:"total_ms,omitempty"`
		Status      int         `json:"status"`
		TLSValid    bool        `json:"tls_valid,omitempty"`
		Captive     bool        `json:"captive"`
		Error       string      `json:"error,omitempty"`
		TargetClass TargetClass `json:"target_class,omitempty"`
		ProbeMethod ProbeMethod `json:"probe_method,omitempty"`
	}

	LinkContext struct {
		Timestamp          int64            `json:"ts"`
		InterfaceType      string           `json:"iface_type"`
		LinkMbps           float64          `json:"link_mbps,omitempty"`
		TXLinkMbps         float64          `json:"tx_link_mbps,omitempty"`
		RXLinkMbps         float64          `json:"rx_link_mbps,omitempty"`
		RSSIDBm            float64          `json:"rssi_dbm,omitempty"`
		NoiseDBm           float64          `json:"noise_dbm,omitempty"`
		SNRDB              float64          `json:"snr_db,omitempty"`
		FrequencyMHz       int              `json:"frequency_mhz,omitempty"`
		Channel            int              `json:"channel,omitempty"`
		ChannelWidthMHz    int              `json:"channel_width_mhz,omitempty"`
		PHY                string           `json:"phy,omitempty"`
		Security           string           `json:"security,omitempty"`
		SSID               string           `json:"ssid,omitempty"`
		Metered            bool             `json:"metered,omitempty"`
		OnBattery          bool             `json:"on_battery,omitempty"`
		BatteryPct         float64          `json:"battery_pct,omitempty"`
		GatewayChanged     bool             `json:"gateway_changed"`
		ContextAvailable   bool             `json:"context_available,omitempty"`
		AssociationChanged bool             `json:"association_changed,omitempty"`
		NetworkID          string           `json:"network_id,omitempty"`
		FirstHopID         string           `json:"first_hop_id,omitempty"`
		Availability       LinkAvailability `json:"availability"`
		RawNetworkIdentity string           `json:"-"`
		RawFirstHop        string           `json:"-"`
	}

	LinkAvailability struct {
		Interface       bool `json:"interface"`
		LinkRate        bool `json:"link_rate"`
		RSSI            bool `json:"rssi"`
		Noise           bool `json:"noise"`
		SNR             bool `json:"snr"`
		Frequency       bool `json:"frequency"`
		Channel         bool `json:"channel"`
		ChannelWidth    bool `json:"channel_width"`
		PHY             bool `json:"phy"`
		Security        bool `json:"security"`
		Power           bool `json:"power"`
		Metered         bool `json:"metered"`
		Association     bool `json:"association"`
		NetworkIdentity bool `json:"network_identity"`
		FirstHop        bool `json:"first_hop"`
	}

	Event struct {
		Timestamp int64          `json:"ts"`
		Type      string         `json:"type"`
		Detail    map[string]any `json:"detail"`
	}

	ThroughputResult struct {
		BytesTransferred uint64  `json:"bytes_transferred"`
		DurationMS       float64 `json:"duration_ms"`
		ThroughputMbps   float64 `json:"throughput_mbps"`
		P75Mbps          float64 `json:"p75_mbps,omitempty"`
		CapHit           bool    `json:"cap_hit"`
		TTFBMS           float64 `json:"ttfb_ms,omitempty"`
		Error            string  `json:"error,omitempty"`
	}

	LatencyResult struct {
		MinMS           float64 `json:"min_ms"`
		AvgMS           float64 `json:"avg_ms"`
		MaxMS           float64 `json:"max_ms"`
		JitterMS        float64 `json:"jitter_ms"`
		PacketLossPct   float64 `json:"packet_loss_pct"`
		PacketLossValid bool    `json:"packet_loss_valid"`
		ProbesSent      uint32  `json:"probes_sent"`
		LoadedAvgMS     float64 `json:"loaded_avg_ms,omitempty"`
		Target          string  `json:"target,omitempty"`
		Error           string  `json:"error,omitempty"`
	}

	LatencySummary struct {
		Count             uint32  `json:"count"`
		Attempts          uint32  `json:"attempts"`
		MinMS             float64 `json:"min_ms"`
		P5MS              float64 `json:"p5_ms"`
		P50MS             float64 `json:"p50_ms"`
		P90MS             float64 `json:"p90_ms"`
		P95MS             float64 `json:"p95_ms"`
		P99MS             float64 `json:"p99_ms"`
		MaxMS             float64 `json:"max_ms"`
		JitterIQRMS       float64 `json:"jitter_iqr_ms"`
		JitterMeanDeltaMS float64 `json:"jitter_mean_delta_ms"`
		PacketLossPct     float64 `json:"packet_loss_pct"`
		PacketLossValid   bool    `json:"packet_loss_valid"`
		Error             string  `json:"error,omitempty"`
	}

	Responsiveness struct {
		Baseline                         LatencySummary    `json:"baseline"`
		DownloadLoaded                   LatencySummary    `json:"download_loaded"`
		UploadLoaded                     LatencySummary    `json:"upload_loaded"`
		BidirectionalLoaded              *LatencySummary   `json:"bidirectional_loaded,omitempty"`
		Cooldown                         *LatencySummary   `json:"cooldown,omitempty"`
		DownloadBloatP90MS               float64           `json:"download_bloat_p90_ms"`
		UploadBloatP90MS                 float64           `json:"upload_bloat_p90_ms"`
		BidirectionalBloatP90MS          float64           `json:"bidirectional_bloat_p90_ms,omitempty"`
		DownloadGrade                    string            `json:"download_grade"`
		UploadGrade                      string            `json:"upload_grade"`
		BidirectionalGrade               string            `json:"bidirectional_grade,omitempty"`
		OverallGrade                     string            `json:"overall_grade"`
		PrimaryDriver                    string            `json:"primary_driver,omitempty"`
		ConfidenceLevel                  string            `json:"confidence_level"`
		ConfidenceReasons                []string          `json:"confidence_reasons"`
		Profile                          string            `json:"profile"`
		BidirectionalCountsTowardOverall bool              `json:"bidirectional_counts_toward_overall,omitempty"`
		BidirectionalDownload            *ThroughputResult `json:"bidirectional_download,omitempty"`
		BidirectionalUpload              *ThroughputResult `json:"bidirectional_upload,omitempty"`
		TotalDownloadBytes               uint64            `json:"total_download_bytes,omitempty"`
		TotalUploadBytes                 uint64            `json:"total_upload_bytes,omitempty"`
	}

	TrafficContext struct {
		CrossTrafficMbps float64 `json:"cross_traffic_mbps"`
		Contaminated     bool    `json:"contaminated"`
		Metered          bool    `json:"metered"`
		MeteredAvailable bool    `json:"metered_available"`
		OnBattery        bool    `json:"on_battery"`
		PowerAvailable   bool    `json:"power_available"`
		BatteryPct       float64 `json:"battery_pct,omitempty"`
		Override         bool    `json:"override,omitempty"`
	}

	SpeedTest struct {
		MeasurementID         string                   `json:"measurement_id"`
		Profile               string                   `json:"profile"`
		Timestamp             int64                    `json:"ts"`
		Download              ThroughputResult         `json:"download"`
		Upload                ThroughputResult         `json:"upload"`
		Latency               LatencyResult            `json:"latency"`
		EndpointHost          string                   `json:"endpoint_host"`
		CPUBusyPct            float64                  `json:"cpu_busy_pct"`
		DurationMS            float64                  `json:"duration_ms"`
		Responsiveness        Responsiveness           `json:"responsiveness"`
		TrafficContext        TrafficContext           `json:"traffic_context"`
		Budget                quality.Budget           `json:"budget"`
		Phases                map[string]PhaseEvidence `json:"phases,omitempty"`
		Paths                 PathResponsiveness       `json:"paths"`
		Experience            quality.Experience       `json:"experience"`
		ExperienceEligible    bool                     `json:"experience_eligible"`
		ExclusionReason       string                   `json:"exclusion_reason,omitempty"`
		MeasurementConfidence quality.Confidence       `json:"measurement_confidence"`
		// Household carries the paced-scenario evidence for household
		// profile results and is absent for saturation-style profiles.
		Household *quality.HouseholdResult `json:"household,omitempty"`
	}

	PhaseEvidence struct {
		MinDurationMS int64                `json:"min_duration_ms"`
		MaxDurationMS int64                `json:"max_duration_ms"`
		StartMS       int64                `json:"start_ms"`
		EndMS         int64                `json:"end_ms"`
		Buckets       []quality.Bucket     `json:"buckets,omitempty"`
		StableRegion  quality.StableRegion `json:"stable_region"`
	}

	PathLatency struct {
		Samples   int     `json:"samples"`
		Successes int     `json:"successes"`
		P50MS     float64 `json:"p50_ms"`
		P95MS     float64 `json:"p95_ms"`
		LossPct   float64 `json:"loss_pct"`
	}

	PathResponsiveness struct {
		Persistent PathLatency `json:"persistent"`
		Fresh      PathLatency `json:"fresh"`
	}

	Batch struct {
		BatchID       string            `json:"batch_id"`
		Sequence      int64             `json:"seq"`
		SensorClock   int64             `json:"sensor_clock"`
		AppVersion    string            `json:"app_version"`
		ConfigVersion int               `json:"config_version"`
		MinuteMetrics []MinuteMetric    `json:"minute_metrics"`
		DNSChecks     []DNSCheck        `json:"dns_checks"`
		HTTPChecks    []HTTPCheck       `json:"http_checks"`
		LinkContext   []LinkContext     `json:"link_context"`
		SpeedTests    []json.RawMessage `json:"speed_tests"`
		Events        []Event           `json:"events"`
	}

	IngestResponse struct {
		ConfigVersion    int               `json:"config_version"`
		Duplicate        bool              `json:"duplicate"`
		RejectedRecords  int               `json:"rejected_records"`
		BufferDepth      int               `json:"buffer_depth"`
		SpeedTestRequest *SpeedTestRequest `json:"speed_test_request,omitempty"`
	}

	ConfigResponse struct {
		Config        SensorConfig `json:"config"`
		ConfigVersion int          `json:"config_version"`
	}

	HeartbeatRequest struct {
		AppVersion    string `json:"app_version"`
		UptimeSeconds int64  `json:"uptime_seconds"`
		SpoolRecords  int64  `json:"spool_records"`
		SpoolBytes    int64  `json:"spool_bytes"`
	}

	HeartbeatResponse struct {
		ConfigVersion    int               `json:"config_version"`
		SpeedTestRequest *SpeedTestRequest `json:"speed_test_request,omitempty"`
	}

	SpeedTestRequest struct {
		Nonce     string `json:"nonce"`
		Profile   string `json:"profile"`
		IssuedAt  int64  `json:"issued_at"`
		ExpiresAt int64  `json:"expires_at"`
		Signature string `json:"signature"`
	}

	// SpeedTestProgress is a transient live snapshot emitted while a test
	// runs. It feeds local UI streaming only and is never spooled or uploaded.
	SpeedTestProgress struct {
		MeasurementID string  `json:"measurement_id"`
		Profile       string  `json:"profile"`
		Phase         string  `json:"phase"`
		ElapsedMS     int64   `json:"elapsed_ms"`
		DownloadMbps  float64 `json:"download_mbps"`
		UploadMbps    float64 `json:"upload_mbps"`
		DownloadBytes uint64  `json:"download_bytes"`
		UploadBytes   uint64  `json:"upload_bytes"`
		RTTMS         float64 `json:"rtt_ms,omitempty"`
		BaselineP50MS float64 `json:"baseline_p50_ms,omitempty"`
		Done          bool    `json:"done,omitempty"`
	}
)

const (
	EnrollPath    = "/api/v2/sensors/enroll/"
	IngestPath    = "/api/v2/sensors/ingest/"
	ConfigPath    = "/api/v2/sensors/config/"
	HeartbeatPath = "/api/v2/sensors/heartbeat/"
	ProbePath     = "/api/v2/sensors/probe/"

	TargetGateway      TargetClass = "gateway"
	TargetController   TargetClass = "controller"
	TargetPublicAnchor TargetClass = "public_anchor"
	TargetCustom       TargetClass = "custom"
	TargetSitePeer     TargetClass = "site_peer"

	ProbeICMP       ProbeMethod = "icmp"
	ProbeTCPConnect ProbeMethod = "tcp_connect"
	ProbeHTTPS      ProbeMethod = "https"

	// ProfileHousehold and ProfileSaturation are the profiles this sensor
	// schedules by default; ProfileContent and ProfileCapacity are the legacy
	// pair older controllers still configure and whose results remain valid.
	ProfileHousehold  = "household"
	ProfileSaturation = "saturation"
	ProfileContent    = "content"
	ProfileCapacity   = "capacity"

	// HouseholdContractVersion is the first controller contract that accepts
	// household profiles, the household mix and the household result block.
	// Older controllers reject unknown top-level keys, so the sensor never
	// schedules or offers household tests against them.
	HouseholdContractVersion = 3
)

// KnownSpeedProfile reports whether name is a profile this build can run.
func KnownSpeedProfile(name string) bool {
	switch name {
	case ProfileHousehold, ProfileSaturation, ProfileContent, ProfileCapacity:
		return true
	}
	return false
}

// SaturationProfile reports whether the profile drives the link to
// capacity. Household tests pace to configured demand instead.
func SaturationProfile(name string) bool {
	return name == ProfileSaturation || name == ProfileCapacity
}

// ExperienceProfile reports whether results from the profile may enter the
// representative everyday history.
func ExperienceProfile(name string) bool {
	return name == ProfileHousehold || name == ProfileContent
}

// DeviceSpeedProfile returns the device-owned ceilings for any known profile,
// including the legacy pair that DefaultConfig no longer schedules.
func DeviceSpeedProfile(name string) (SpeedProfileConfig, bool) {
	for _, profile := range append(DefaultConfig().SpeedProfiles, legacySpeedProfiles()...) {
		if profile.Name == name {
			return profile, true
		}
	}
	return SpeedProfileConfig{}, false
}

// legacySpeedProfiles are the pre-household defaults, kept for controllers
// that still speak contract version 2.
func legacySpeedProfiles() []SpeedProfileConfig {
	return []SpeedProfileConfig{
		{Name: ProfileContent, Enabled: true, Windows: []string{"00:00-24:00"}, CadenceMinutes: 360, MinDurationSeconds: 4, MaxDurationSeconds: 20, MaxDownloadBytes: 256 << 20, MaxUploadBytes: 96 << 20, MaxConcurrentRequests: 4, MinSpacingMinutes: 15, CountsTowardExperience: true},
		{Name: ProfileCapacity, Enabled: false, Windows: []string{"never"}, MinDurationSeconds: 8, MaxDurationSeconds: 60, MaxDownloadBytes: 512 << 20, MaxUploadBytes: 256 << 20, MaxConcurrentRequests: 8, MinSpacingMinutes: 60, CountsTowardExperience: false},
	}
}

func DefaultConfig() SensorConfig {
	return SensorConfig{
		ContractVersion:          HouseholdContractVersion,
		ProbeIntervalSeconds:     1,
		UploadIntervalSeconds:    60,
		DNSIntervalSeconds:       60,
		HTTPIntervalSeconds:      60,
		HeartbeatIntervalSeconds: 600,
		ProbeTargets:             []string{"gateway", "controller", "anchor:1.1.1.1", "anchor:8.8.8.8"},
		DNSDomains:               []string{"example.com"},
		DNSResolvers:             []string{"1.1.1.1"},
		HTTPTargets:              []string{"https://www.google.com/generate_204", "https://connectivitycheck.gstatic.com/generate_204"},
		// Four paced household tests plus one daily saturation test must fit
		// the ledgers on a fast link; the ledgers remain the hard bound.
		DailyDataBudgetMB:          4096,
		MonthlyDataBudgetMB:        61440,
		SpeedTestWindows:           []string{"00:00-06:00", "06:00-12:00", "12:00-18:00", "18:00-24:00"},
		SpeedTestCrossTrafficMbps:  5,
		SpeedTestMinSpacingMinutes: 15,
		SpeedProfiles: []SpeedProfileConfig{
			// Household paces activities to their configured demand, so its
			// ceiling (300 MB down plus up) is a cap it rarely approaches. The
			// minimum duration is the shortest observation window worth scoring.
			{Name: ProfileHousehold, Enabled: true, Windows: []string{"00:00-24:00"}, CadenceMinutes: 360, MinDurationSeconds: 8, MaxDurationSeconds: 30, MaxDownloadBytes: quality.HouseholdDefaultDownloadCap, MaxUploadBytes: quality.HouseholdDefaultUploadCap, MaxConcurrentRequests: 8, MinSpacingMinutes: 60, CountsTowardExperience: true},
			// Saturation byte limits are ceilings, not targets: the runner sizes
			// each phase from the measured link rate and stops early at
			// stability, so slow links spend little while fast links get enough
			// bytes to grade. It runs once a day so the history also shows what
			// happens when the line is driven to its limit.
			{Name: ProfileSaturation, Enabled: true, Windows: []string{"00:00-24:00"}, CadenceMinutes: 1440, MinDurationSeconds: 8, MaxDurationSeconds: 60, MaxDownloadBytes: 1 << 30, MaxUploadBytes: 512 << 20, MaxConcurrentRequests: 8, MinSpacingMinutes: 360, CountsTowardExperience: false},
		},
	}
}

// EffectiveHouseholdMix resolves the mix a household test reproduces: a local
// override wins, then controller configuration, then the catalog default.
// The source is recorded on every result so history can explain itself.
func EffectiveHouseholdMix(local, configured *quality.HouseholdMix) (quality.HouseholdMix, string) {
	if local != nil {
		return quality.NormalizeHouseholdMix(*local), quality.MixSourceLocal
	}
	if configured != nil {
		return quality.NormalizeHouseholdMix(*configured), quality.MixSourceController
	}
	return quality.DefaultHouseholdMix(), quality.MixSourceDefault
}

// NormalizeConfig treats the controller as untrusted input and applies the
// same bounds as the server before a profile reaches the scheduler.
func NormalizeConfig(in SensorConfig) SensorConfig {
	out := in
	out.ContractVersion = bounded(in.ContractVersion, 1, 1000, 2)
	out.ProbeIntervalSeconds = bounded(in.ProbeIntervalSeconds, 1, 60, 1)
	out.UploadIntervalSeconds = bounded(in.UploadIntervalSeconds, 30, 900, 60)
	out.DNSIntervalSeconds = bounded(in.DNSIntervalSeconds, 30, 3600, 60)
	out.HTTPIntervalSeconds = bounded(in.HTTPIntervalSeconds, 30, 3600, 60)
	out.HeartbeatIntervalSeconds = bounded(in.HeartbeatIntervalSeconds, 300, 3600, 600)
	out.DailyDataBudgetMB = bounded(in.DailyDataBudgetMB, 1, 8192, 4096)
	out.MonthlyDataBudgetMB = bounded(in.MonthlyDataBudgetMB, 1, 131072, 61440)
	out.SpeedTestMinSpacingMinutes = bounded(in.SpeedTestMinSpacingMinutes, 15, 1440, 15)
	if out.SpeedTestCrossTrafficMbps < 0 || out.SpeedTestCrossTrafficMbps > 10000 {
		out.SpeedTestCrossTrafficMbps = 5
	}
	out.ProbeTargets = normalizeProbeTargets(in.ProbeTargets)
	out.DNSDomains = boundedStrings(in.DNSDomains, 16, 253, DefaultConfig().DNSDomains)
	out.DNSResolvers = normalizeDNSResolvers(in.DNSResolvers)
	out.HTTPTargets = normalizeHTTPTargets(in.HTTPTargets)
	out.SpeedTestWindows = normalizeSpeedWindows(in.SpeedTestWindows, DefaultConfig().SpeedTestWindows)
	out.SpeedProfiles = normalizeSpeedProfiles(in.SpeedProfiles, out.SpeedTestWindows, out.ContractVersion)
	out.HouseholdMix = nil
	if in.HouseholdMix != nil && out.ContractVersion >= HouseholdContractVersion {
		mix := quality.NormalizeHouseholdMix(*in.HouseholdMix)
		out.HouseholdMix = &mix
	}
	return out
}

// normalizeSpeedWindows drops window strings the scheduler cannot parse.
// Without this, a malformed remote window list passes length bounding but
// yields zero runnable intervals, permanently disabling scheduling while
// status keeps advertising a next run that never starts.
func normalizeSpeedWindows(values, fallback []string) []string {
	values = boundedStrings(values, 12, 11, fallback)
	valid := make([]string, 0, len(values))
	for _, value := range values {
		if validSpeedWindow(value) {
			valid = append(valid, value)
		}
	}
	if len(valid) == 0 {
		return append([]string(nil), fallback...)
	}
	return valid
}

// validSpeedWindow accepts the scheduler's window grammar: the "never"
// sentinel or "HH:MM-HH:MM" with distinct endpoints, mirroring
// measurement.cadenceSchedule's parsing rules.
func validSpeedWindow(value string) bool {
	if value == "never" {
		return true
	}
	start, end, ok := strings.Cut(value, "-")
	if !ok {
		return false
	}
	startMinutes, startOK := windowClockMinutes(start)
	endMinutes, endOK := windowClockMinutes(end)
	return startOK && endOK && startMinutes != endMinutes
}

func windowClockMinutes(value string) (int, bool) {
	hourText, minuteText, ok := strings.Cut(value, ":")
	if !ok {
		return 0, false
	}
	hour, hourErr := strconv.Atoi(hourText)
	minute, minuteErr := strconv.Atoi(minuteText)
	if hourErr != nil || minuteErr != nil || minute < 0 || minute > 59 || hour < 0 || hour > 24 || (hour == 24 && minute != 0) {
		return 0, false
	}
	return hour*60 + minute, true
}

func normalizeProbeTargets(values []string) []string {
	values = boundedStrings(values, 8, 255, DefaultConfig().ProbeTargets)
	for _, value := range values {
		address := value
		if value == "gateway" || value == "controller" {
			continue
		}
		if after, ok := strings.CutPrefix(value, "anchor:"); ok {
			address = after
		}
		if address == "" || strings.Contains(address, "://") || strings.ContainsAny(address, " /?#@\t\r\n") {
			return append([]string(nil), DefaultConfig().ProbeTargets...)
		}
		if _, _, err := net.SplitHostPort(address); err == nil || !IsSafePublicHost(address) {
			return append([]string(nil), DefaultConfig().ProbeTargets...)
		}
	}
	return values
}

func normalizeHTTPTargets(values []string) []string {
	values = boundedStrings(values, 16, 2048, DefaultConfig().HTTPTargets)
	for _, value := range values {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" || (parsed.Port() != "" && parsed.Port() != "443") || !IsSafePublicHost(parsed.Hostname()) {
			return append([]string(nil), DefaultConfig().HTTPTargets...)
		}
	}
	return values
}

// normalizeSpeedProfiles bounds every controller-supplied profile and fills
// in the defaults the controller's contract supports. A contract-2 controller
// only knows content and capacity, so household and saturation are neither
// accepted from it nor appended for it: its ingest would reject the results.
func normalizeSpeedProfiles(values []SpeedProfileConfig, legacyWindows []string, contractVersion int) []SpeedProfileConfig {
	household := contractVersion >= HouseholdContractVersion
	defaults := legacySpeedProfiles()
	if household {
		defaults = DefaultConfig().SpeedProfiles
	}
	if len(values) == 0 {
		if !household {
			defaults[0].Windows = append([]string(nil), legacyWindows...)
		}
		return defaults
	}
	if len(values) > 4 {
		values = values[:4]
	}
	result := make([]SpeedProfileConfig, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		if !KnownSpeedProfile(value.Name) || seen[value.Name] {
			continue
		}
		if !household && (value.Name == ProfileHousehold || value.Name == ProfileSaturation) {
			continue
		}
		seen[value.Name] = true
		fallback, _ := DeviceSpeedProfile(value.Name)
		value.MinDurationSeconds = bounded(value.MinDurationSeconds, 2, 30, fallback.MinDurationSeconds)
		value.MaxDurationSeconds = bounded(value.MaxDurationSeconds, value.MinDurationSeconds, 120, fallback.MaxDurationSeconds)
		// The fallback maximum can undercut a valid configured minimum.
		value.MaxDurationSeconds = max(value.MaxDurationSeconds, value.MinDurationSeconds)
		if value.Name == ProfileHousehold {
			// The household engine refuses anything shorter than 8 s; a
			// controller must not be able to schedule a profile that can
			// never run and burns its reservation every slot.
			value.MaxDurationSeconds = max(value.MaxDurationSeconds, 8)
			value.MinDurationSeconds = min(value.MinDurationSeconds, value.MaxDurationSeconds)
		}
		value.MaxDownloadBytes = boundedUint64(value.MaxDownloadBytes, 512<<10, 1<<30, fallback.MaxDownloadBytes)
		value.MaxUploadBytes = boundedUint64(value.MaxUploadBytes, 256<<10, 512<<20, fallback.MaxUploadBytes)
		value.MaxConcurrentRequests = bounded(value.MaxConcurrentRequests, 1, 8, fallback.MaxConcurrentRequests)
		value.MinSpacingMinutes = bounded(value.MinSpacingMinutes, 15, 10080, fallback.MinSpacingMinutes)
		if value.CadenceMinutes != 0 {
			value.CadenceMinutes = bounded(value.CadenceMinutes, 60, 1440, fallback.CadenceMinutes)
		}
		value.Windows = normalizeSpeedWindows(value.Windows, fallback.Windows)
		// These are device-owned safety and interpretation decisions. Remote
		// config may schedule less work, but cannot opt the user into metered or
		// low-battery tests or turn capacity tests into home-experience scores.
		value.AllowMetered = false
		value.AllowOnBattery = false
		value.CountsTowardExperience = ExperienceProfile(value.Name)
		result = append(result, value)
	}
	if len(result) == 0 {
		return defaults
	}
	for index, fallback := range defaults {
		if !seen[fallback.Name] {
			if index == 0 && !household {
				fallback.Windows = append([]string(nil), legacyWindows...)
			}
			result = append(result, fallback)
		}
	}
	return result
}

func normalizeDNSResolvers(values []string) []string {
	values = boundedStrings(values, 4, 64, DefaultConfig().DNSResolvers)
	for _, value := range values {
		if net.ParseIP(value) == nil || !IsPublicIP(net.ParseIP(value)) {
			return append([]string(nil), DefaultConfig().DNSResolvers...)
		}
	}
	return values
}

func boundedUint64(value, low, high, fallback uint64) uint64 {
	if value < low || value > high {
		return fallback
	}
	return value
}

func bounded(value, low, high, fallback int) int {
	if value < low || value > high {
		return fallback
	}
	return value
}

func boundedStrings(values []string, maximum, maxLength int, fallback []string) []string {
	if len(values) == 0 || len(values) > maximum {
		return append([]string(nil), fallback...)
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || len(value) > maxLength {
			return append([]string(nil), fallback...)
		}
		out = append(out, value)
	}
	return out
}
