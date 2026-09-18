//go:build darwin

package measurement

import (
	"testing"
)

func TestParseDarwinAirPortUsesOnlyCurrentNetwork(t *testing.T) {
	t.Parallel()
	input := []byte(`{
		"SPAirPortDataType": [{
			"spairport_airport_interfaces": [{
				"_name": "en0",
				"spairport_current_network_information": {
					"_name": "Current WiFi",
					"spairport_signal_noise": "-51 dBm / -92 dBm",
					"spairport_network_channel": "149 (5GHz, 80MHz)",
					"spairport_network_rate": "1200",
					"spairport_network_phymode": "802.11ax"
				},
				"spairport_other_local_wireless_networks": [{
					"_name": "Nearby WiFi",
					"spairport_signal_noise": "-20 dBm / -92 dBm",
					"spairport_network_channel": "1"
				}]
			}]
		}]
	}`)
	context, err := parseDarwinAirPort(input, true)
	if err != nil {
		t.Fatal(err)
	}
	if context.InterfaceType != "wifi" || context.SSID != "Current WiFi" || context.RSSIDBm != -51 || context.Channel != 149 || context.LinkMbps != 1200 || context.PHY != "802.11ax" {
		t.Fatalf("context = %+v", context)
	}
	if context.NoiseDBm != -92 || context.SNRDB != 41 {
		t.Fatalf("noise/snr = %+v", context)
	}
}

func TestParseDarwinAirPortOmitsSSIDWithoutConsent(t *testing.T) {
	t.Parallel()
	input := []byte(`{"SPAirPortDataType":[{"spairport_airport_interfaces":[{"spairport_current_network_information":{"_name":"Private WiFi","spairport_network_signal":"-51 dBm"}}]}]}`)
	context, err := parseDarwinAirPort(input, false)
	if err != nil {
		t.Fatal(err)
	}
	if context.InterfaceType != "wifi" || context.SSID != "" || context.RSSIDBm != -51 {
		t.Fatalf("context = %+v", context)
	}
}

func TestApplyDarwinPowerParsesBatteryState(t *testing.T) {
	t.Parallel()
	var context LocalContext
	applyDarwinPower(&context, "Now drawing from 'Battery Power'\n -InternalBattery-0 73%; discharging")
	if !context.OnBattery || context.BatteryPct != 73 {
		t.Fatalf("context = %+v", context)
	}
}

func TestCoreWLANEnumMappings(t *testing.T) {
	t.Parallel()
	if got := corewlanChannelWidthMHz(3); got != 80 {
		t.Fatalf("width(3) = %d, want 80", got)
	}
	if got := corewlanChannelWidthMHz(9); got != 0 {
		t.Fatalf("unknown width = %d, want 0", got)
	}
	for _, test := range []struct{ band, channel, want int }{
		{1, 6, 2437}, {1, 14, 2484}, {2, 44, 5220}, {3, 37, 6135}, {0, 44, 0}, {1, 40, 0},
	} {
		if got := corewlanBandFrequencyMHz(test.band, test.channel); got != test.want {
			t.Fatalf("freq(band=%d, ch=%d) = %d, want %d", test.band, test.channel, got, test.want)
		}
	}
	if got := corewlanPHYName(6); got != "802.11ax" {
		t.Fatalf("phy(6) = %q", got)
	}
	if got := corewlanSecurityName(11); got != "wpa3_personal" {
		t.Fatalf("security(11) = %q", got)
	}
	if got := corewlanSecurityName(99); got != "" {
		t.Fatalf("unknown security = %q, want empty", got)
	}
}
