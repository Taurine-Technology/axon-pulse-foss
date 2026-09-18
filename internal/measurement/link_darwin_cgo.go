//go:build darwin && cgo

package measurement

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework CoreWLAN -framework Foundation
#include "corewlan_darwin.h"
*/
import "C" //nolint:grouper // cgo requires import "C" to stand alone, directly after its preamble.

//nolint:grouper // the standalone import "C" above forces a second declaration.
import (
	"errors"
)

// coreWLANContext reads the associated Wi-Fi state natively. Unlike
// system_profiler on modern macOS, CoreWLAN reports signal, channel, PHY, and
// security without Location Services; only SSID/BSSID stay redacted.
func coreWLANContext(includeSSID bool) (LocalContext, error) {
	var info C.AxonWifiInfo
	if C.axon_corewlan_info(&info) != 0 || info.hasInterface == 0 {
		return LocalContext{}, errors.New("no powered Wi-Fi interface")
	}
	if info.associated == 0 {
		// A powered but unassociated Wi-Fi interface: the machine is online
		// through something else, so let the profiler/ethernet path decide.
		return LocalContext{}, errors.New("wifi interface not associated")
	}
	out := LocalContext{InterfaceType: "wifi", ContextAvailable: true}
	if info.rssi < 0 {
		out.RSSIDBm = float64(info.rssi)
	}
	if info.noise < 0 {
		out.NoiseDBm = float64(info.noise)
	}
	if out.RSSIDBm < 0 && out.NoiseDBm < 0 {
		out.SNRDB = max(out.RSSIDBm-out.NoiseDBm, 0)
	}
	out.TXLinkMbps = float64(info.txRate)
	out.LinkMbps = float64(info.txRate)
	out.Channel = int(info.channel)
	out.ChannelWidthMHz = corewlanChannelWidthMHz(int(info.width))
	out.FrequencyMHz = corewlanBandFrequencyMHz(int(info.band), int(info.channel))
	out.PHY = corewlanPHYName(int(info.phyMode))
	out.Security = corewlanSecurityName(int(info.security))
	ssid := C.GoString(&info.ssid[0])
	bssid := C.GoString(&info.bssid[0])
	out.NetworkIdentity = bssid
	if out.NetworkIdentity == "" && includeSSID {
		out.NetworkIdentity = ssid
	}
	if includeSSID {
		out.SSID = ssid
	}
	return out, nil
}
