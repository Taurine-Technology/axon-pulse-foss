//go:build darwin

package measurement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Taurine-Technology/axon-pulse/internal/prober"
)

type (
	darwinAirPortDocument struct {
		Data []darwinAirPortData `json:"SPAirPortDataType"`
	}

	darwinAirPortData struct {
		Interfaces []darwinAirPortInterface `json:"spairport_airport_interfaces"`
	}

	darwinAirPortInterface struct {
		CurrentNetwork map[string]any `json:"spairport_current_network_information"`
	}
)

func platformContext(ctx context.Context, includeSSID bool) (LocalContext, error) {
	// CoreWLAN reads signal, channel, PHY, and security directly from the
	// Wi-Fi subsystem without Location Services (which modern macOS requires
	// before system_profiler will show any current-network details at all).
	// It needs cgo, so the statically built CLI falls through to the
	// system_profiler parser below.
	out, err := coreWLANContext(includeSSID)
	if err != nil || !out.ContextAvailable {
		out, err = airportProfilerContext(ctx, includeSSID)
		if err != nil {
			return LocalContext{}, err
		}
	}
	out.Gateway, _ = prober.DefaultGateway()
	powerCtx, cancelPower := context.WithTimeout(ctx, 5*time.Second)
	defer cancelPower()
	if power, powerErr := exec.CommandContext(powerCtx, "/usr/bin/pmset", "-g", "batt").Output(); powerErr == nil {
		out.PowerAvailable = true
		applyDarwinPower(&out, string(power))
	}
	return out, nil
}

func airportProfilerContext(ctx context.Context, includeSSID bool) (LocalContext, error) {
	// system_profiler routinely takes several seconds; bound it so a hung
	// invocation cannot permanently stall link-context collection.
	profilerCtx, cancelProfiler := context.WithTimeout(ctx, 15*time.Second)
	defer cancelProfiler()
	output, err := exec.CommandContext(profilerCtx, "/usr/sbin/system_profiler", "SPAirPortDataType", "-json", "-detailLevel", "mini").Output()
	if err != nil {
		return LocalContext{}, err
	}
	return parseDarwinAirPort(output, includeSSID)
}

// corewlanChannelWidthMHz maps CWChannelWidth enum values to megahertz.
func corewlanChannelWidthMHz(width int) int {
	switch width {
	case 1:
		return 20
	case 2:
		return 40
	case 3:
		return 80
	case 4:
		return 160
	default:
		return 0
	}
}

// corewlanBandFrequencyMHz derives the channel's centre frequency from the
// CWChannelBand enum and channel number.
func corewlanBandFrequencyMHz(band, channel int) int {
	switch band {
	case 1: // 2.4 GHz
		if channel == 14 {
			return 2484
		}
		if channel >= 1 && channel <= 13 {
			return 2407 + channel*5
		}
	case 2: // 5 GHz
		if channel > 0 {
			return 5000 + channel*5
		}
	case 3: // 6 GHz
		if channel > 0 {
			return 5950 + channel*5
		}
	}
	return 0
}

// corewlanPHYName maps CWPHYMode enum values to IEEE notation.
func corewlanPHYName(mode int) string {
	switch mode {
	case 1:
		return "802.11a"
	case 2:
		return "802.11b"
	case 3:
		return "802.11g"
	case 4:
		return "802.11n"
	case 5:
		return "802.11ac"
	case 6:
		return "802.11ax"
	case 7:
		return "802.11be"
	default:
		return ""
	}
}

// corewlanSecurityName maps CWSecurity enum values to short labels.
func corewlanSecurityName(security int) string {
	switch security {
	case 0:
		return "none"
	case 1:
		return "wep"
	case 2, 3:
		return "wpa_personal"
	case 4, 5:
		return "wpa2_personal"
	case 6:
		return "dynamic_wep"
	case 7, 8:
		return "wpa_enterprise"
	case 9, 10:
		return "wpa2_enterprise"
	case 11:
		return "wpa3_personal"
	case 12:
		return "wpa3_enterprise"
	case 13:
		return "wpa3_transition"
	case 14, 15:
		return "owe"
	default:
		return ""
	}
}

func parseDarwinAirPort(data []byte, includeSSID bool) (LocalContext, error) {
	var document darwinAirPortDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return LocalContext{}, err
	}
	if len(document.Data) == 0 {
		return LocalContext{}, errors.New("link context unavailable")
	}
	out := LocalContext{InterfaceType: "ethernet", ContextAvailable: true}
	for _, dataType := range document.Data {
		for _, networkInterface := range dataType.Interfaces {
			current := networkInterface.CurrentNetwork
			if len(current) == 0 {
				continue
			}
			out.InterfaceType = "wifi"
			out.NetworkIdentity = scalarString(current["spairport_network_bssid"])
			if out.NetworkIdentity == "" && includeSSID {
				out.NetworkIdentity = scalarString(current["_name"])
			}
			if includeSSID {
				out.SSID = scalarString(current["_name"])
			}
			// macOS reports "-51 dBm / -92 dBm" under spairport_signal_noise;
			// spairport_network_signal is kept as a fallback for older layouts.
			signal := scalarString(current["spairport_signal_noise"])
			if signal == "" {
				signal = scalarString(current["spairport_network_signal"])
			}
			out.RSSIDBm, _ = leadingFloat(signal)
			if _, noise, ok := strings.Cut(signal, "/"); ok {
				out.NoiseDBm, _ = leadingFloat(strings.TrimSpace(noise))
				if out.RSSIDBm < 0 && out.NoiseDBm < 0 {
					out.SNRDB = max(out.RSSIDBm-out.NoiseDBm, 0)
				}
			}
			channelText := scalarString(current["spairport_network_channel"])
			channel, _ := leadingFloat(channelText)
			out.Channel = int(channel)
			out.FrequencyMHz = darwinFrequency(out.Channel, channelText)
			out.ChannelWidthMHz = darwinChannelWidth(channelText)
			out.LinkMbps, _ = leadingFloat(scalarString(current["spairport_network_rate"]))
			out.TXLinkMbps, out.RXLinkMbps = out.LinkMbps, out.LinkMbps
			out.PHY = scalarString(current["spairport_network_phymode"])
			out.Security = scalarString(current["spairport_security_mode"])
			return out, nil
		}
	}
	return out, nil
}

func darwinFrequency(channel int, detail string) int {
	lower := strings.ToLower(detail)
	switch {
	case strings.Contains(lower, "6ghz") && channel > 0:
		return 5950 + 5*channel
	case strings.Contains(lower, "5ghz") && channel > 0:
		return 5000 + 5*channel
	case channel == 14:
		return 2484
	case channel >= 1 && channel <= 13:
		return 2407 + 5*channel
	default:
		return 0
	}
}

func darwinChannelWidth(detail string) int {
	for _, field := range strings.FieldsFunc(strings.ToLower(detail), func(r rune) bool {
		return r == ' ' || r == ',' || r == '(' || r == ')'
	}) {
		if !strings.HasSuffix(field, "mhz") {
			continue
		}
		value, err := strconv.Atoi(strings.TrimSuffix(field, "mhz"))
		if err == nil && value >= 5 && value <= 320 {
			return value
		}
	}
	return 0
}

func applyDarwinPower(out *LocalContext, output string) {
	lower := strings.ToLower(output)
	out.OnBattery = strings.Contains(lower, "'battery power'") || strings.Contains(lower, "\"battery power\"")
	for field := range strings.FieldsSeq(output) {
		field = strings.Trim(field, ";,()")
		if !strings.HasSuffix(field, "%") {
			continue
		}
		percentage, err := strconv.ParseFloat(strings.TrimSuffix(field, "%"), 64)
		if err == nil && percentage >= 0 && percentage <= 100 {
			out.BatteryPct = percentage
			return
		}
	}
}

func leadingFloat(value string) (float64, error) {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return 0, errors.New("numeric value unavailable")
	}
	return strconv.ParseFloat(fields[0], 64)
}

func scalarString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	case nil:
		return ""
	default:
		return fmt.Sprint(typed)
	}
}
