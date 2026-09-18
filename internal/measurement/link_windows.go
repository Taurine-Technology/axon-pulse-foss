//go:build windows

package measurement

import (
	"context"
	"net"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/Taurine-Technology/axon-pulse/internal/prober"
)

type (
	systemPowerStatus struct {
		ACLineStatus, BatteryFlag, BatteryLifePercent, SystemStatusFlag byte
		BatteryLifeTime, BatteryFullLifeTime                            uint32
	}

	// wlanInterfaceInfoList mirrors the fixed header of WLAN_INTERFACE_INFO_LIST;
	// NumberOfItems wlanInterfaceInfo records follow it in the WlanAPI-owned buffer.
	wlanInterfaceInfoList struct {
		NumberOfItems uint32
		Index         uint32
	}

	wlanInterfaceInfo struct {
		GUID        syscall.GUID
		Description [256]uint16
		State       uint32
	}

	dot11SSID struct {
		Length uint32
		Value  [32]byte
	}

	wlanAssociationAttributes struct {
		SSID          dot11SSID
		BSSType       uint32
		BSSID         [6]byte
		Padding       [2]byte
		PHYType       uint32
		PHYIndex      uint32
		SignalQuality uint32
		RXRateKbps    uint32
		TXRateKbps    uint32
	}

	wlanSecurityAttributes struct {
		SecurityEnabled uint32
		OneXEnabled     uint32
		AuthAlgorithm   uint32
		CipherAlgorithm uint32
	}

	wlanConnectionAttributes struct {
		State       uint32
		Mode        uint32
		ProfileName [256]uint16
		Association wlanAssociationAttributes
		Security    wlanSecurityAttributes
	}
)

const (
	wlanInterfaceStateConnected = 1
	wlanOpcodeCurrentConnection = 7
	wlanOpcodeChannelNumber     = 8
)

func platformContext(ctx context.Context, includeSSID bool) (LocalContext, error) {
	out := LocalContext{InterfaceType: "ethernet", ContextAvailable: true}
	out.Gateway, _ = prober.DefaultGateway()
	collectWindowsWLAN(&out, includeSSID)
	status := systemPowerStatus{}
	procedure := syscall.NewLazyDLL("kernel32.dll").NewProc("GetSystemPowerStatus")
	if result, _, _ := procedure.Call(uintptr(unsafe.Pointer(&status))); result != 0 {
		out.PowerAvailable = true
		out.OnBattery = status.ACLineStatus == 0
		if status.BatteryLifePercent <= 100 {
			out.BatteryPct = float64(status.BatteryLifePercent)
		}
	}
	out.Metered, out.MeteredAvailable = cachedMetered(time.Now(), out.NetworkIdentity, func() (bool, bool) {
		return windowsMetered(ctx)
	})
	return out, nil
}

func windowsMetered(ctx context.Context) (bool, bool) {
	commandCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	const script = `[Windows.Networking.Connectivity.NetworkInformation,Windows.Networking.Connectivity,ContentType=WindowsRuntime] | Out-Null; $p=[Windows.Networking.Connectivity.NetworkInformation]::GetInternetConnectionProfile(); if ($null -ne $p) { $p.GetConnectionCost().NetworkCostType }`
	output, err := exec.CommandContext(commandCtx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if err != nil {
		return false, false
	}
	return parseMeteredValue(string(output))
}

func collectWindowsWLAN(out *LocalContext, includeSSID bool) {
	library := syscall.NewLazyDLL("wlanapi.dll")
	openHandle := library.NewProc("WlanOpenHandle")
	closeHandle := library.NewProc("WlanCloseHandle")
	enumInterfaces := library.NewProc("WlanEnumInterfaces")
	queryInterface := library.NewProc("WlanQueryInterface")
	freeMemory := library.NewProc("WlanFreeMemory")
	var negotiated uint32
	var handle uintptr
	result, _, _ := openHandle.Call(2, 0, uintptr(unsafe.Pointer(&negotiated)), uintptr(unsafe.Pointer(&handle)))
	if result != 0 {
		return
	}
	defer func() { _, _, _ = closeHandle.Call(handle, 0) }()
	// The list and query results are WlanAPI-owned buffers returned through
	// out-pointers. Keeping them as typed pointers (not uintptr) keeps the
	// uintptr->pointer conversions vet-clean; WlanFreeMemory releases them.
	var list *wlanInterfaceInfoList
	result, _, _ = enumInterfaces.Call(handle, 0, uintptr(unsafe.Pointer(&list)))
	if result != 0 || list == nil {
		return
	}
	defer func() { _, _, _ = freeMemory.Call(uintptr(unsafe.Pointer(list))) }()
	first := unsafe.Add(unsafe.Pointer(list), unsafe.Sizeof(wlanInterfaceInfoList{}))
	size := unsafe.Sizeof(wlanInterfaceInfo{})
	for index := range list.NumberOfItems {
		info := (*wlanInterfaceInfo)(unsafe.Add(first, uintptr(index)*size))
		if info.State != wlanInterfaceStateConnected {
			continue
		}
		var dataSize, valueType uint32
		var data *byte
		result, _, _ = queryInterface.Call(handle, uintptr(unsafe.Pointer(&info.GUID)), wlanOpcodeCurrentConnection, 0, uintptr(unsafe.Pointer(&dataSize)), uintptr(unsafe.Pointer(&data)), uintptr(unsafe.Pointer(&valueType)))
		if result != 0 || data == nil {
			continue
		}
		if dataSize < uint32(unsafe.Sizeof(wlanConnectionAttributes{})) {
			_, _, _ = freeMemory.Call(uintptr(unsafe.Pointer(data)))
			continue
		}
		attributes := (*wlanConnectionAttributes)(unsafe.Pointer(data))
		out.InterfaceType = "wifi"
		// Colon-separated hex matches the Linux BSSID format; an all-zero
		// BSSID carries no association evidence and must stay absent.
		if bssid := attributes.Association.BSSID; bssid != [6]byte{} {
			out.NetworkIdentity = net.HardwareAddr(bssid[:]).String()
		}
		if includeSSID {
			length := min(int(attributes.Association.SSID.Length), len(attributes.Association.SSID.Value))
			out.SSID = strings.ToValidUTF8(string(attributes.Association.SSID.Value[:length]), "")
		}
		out.RSSIDBm = float64(attributes.Association.SignalQuality)/2 - 100
		out.RXLinkMbps = float64(attributes.Association.RXRateKbps) / 1000
		out.TXLinkMbps = float64(attributes.Association.TXRateKbps) / 1000
		out.LinkMbps = max(out.RXLinkMbps, out.TXLinkMbps)
		out.PHY = windowsPHY(attributes.Association.PHYType)
		out.Security = windowsSecurity(attributes.Security)
		_, _, _ = freeMemory.Call(uintptr(unsafe.Pointer(data)))
		data = nil
		dataSize = 0
		result, _, _ = queryInterface.Call(handle, uintptr(unsafe.Pointer(&info.GUID)), wlanOpcodeChannelNumber, 0, uintptr(unsafe.Pointer(&dataSize)), uintptr(unsafe.Pointer(&data)), uintptr(unsafe.Pointer(&valueType)))
		if result == 0 && data != nil {
			if dataSize >= 4 {
				out.Channel = int(*(*uint32)(unsafe.Pointer(data)))
			}
			_, _, _ = freeMemory.Call(uintptr(unsafe.Pointer(data)))
		}
		return
	}
}

func windowsSecurity(value wlanSecurityAttributes) string {
	if value.SecurityEnabled == 0 {
		return "open"
	}
	if value.OneXEnabled != 0 {
		return "enterprise"
	}
	return "secured"
}

func windowsPHY(value uint32) string {
	switch value {
	case 4:
		return "802.11a"
	case 5:
		return "802.11b"
	case 6:
		return "802.11g"
	case 7:
		return "802.11n"
	case 8:
		return "802.11ac"
	case 10:
		return "802.11ax"
	default:
		return "unknown"
	}
}
