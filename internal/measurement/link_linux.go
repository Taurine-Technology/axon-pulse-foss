//go:build linux

package measurement

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mdlayher/wifi"
)

func platformContext(ctx context.Context, includeSSID bool) (LocalContext, error) {
	iface, gateway := linuxDefaultRoute()
	if iface == "" {
		return LocalContext{}, errors.New("default interface unavailable")
	}
	out := LocalContext{InterfaceType: "ethernet", Gateway: gateway, ContextAvailable: true}
	if wireless, ok := linuxWirelessContext(iface, includeSSID); ok {
		out.InterfaceType, out.RSSIDBm, out.Channel, out.FrequencyMHz, out.SSID, out.NetworkIdentity = "wifi", wireless.RSSIDBm, wireless.Channel, wireless.FrequencyMHz, wireless.SSID, wireless.NetworkIdentity
	} else if wirelessRSSI, ok := linuxWirelessRSSI(iface); ok {
		out.InterfaceType, out.RSSIDBm = "wifi", wirelessRSSI
	}
	if raw, err := os.ReadFile(filepath.Join("/sys/class/net", iface, "speed")); err == nil {
		out.LinkMbps, _ = strconv.ParseFloat(strings.TrimSpace(string(raw)), 64)
	}
	out.OnBattery, out.BatteryPct, out.PowerAvailable = linuxBattery()
	out.Metered, out.MeteredAvailable = cachedMetered(time.Now(), iface+"|"+out.NetworkIdentity, func() (bool, bool) {
		return linuxMetered(ctx, iface)
	})
	return out, nil
}

func linuxMetered(ctx context.Context, iface string) (bool, bool) {
	commandCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	command := exec.CommandContext(commandCtx, "nmcli", "-g", "GENERAL.METERED", "device", "show", iface)
	// Force untranslated output so parseMeteredValue sees the canonical states.
	command.Env = append(os.Environ(), "LC_ALL=C")
	output, err := command.Output()
	if err != nil {
		return false, false
	}
	return parseMeteredValue(string(output))
}

type (
	linuxWireless struct {
		RSSIDBm         float64
		Channel         int
		FrequencyMHz    int
		SSID            string
		NetworkIdentity string
	}
)

func linuxWirelessContext(name string, includeSSID bool) (linuxWireless, bool) {
	client, err := wifi.New()
	if err != nil {
		return linuxWireless{}, false
	}
	defer func() { _ = client.Close() }()
	interfaces, err := client.Interfaces()
	if err != nil {
		return linuxWireless{}, false
	}
	for _, iface := range interfaces {
		if iface.Name != name {
			continue
		}
		bss, err := client.BSS(iface)
		if err != nil {
			return linuxWireless{}, false
		}
		wireless := linuxWireless{RSSIDBm: float64(bss.Signal) / 100, Channel: wifiChannel(bss.Frequency), FrequencyMHz: bss.Frequency, NetworkIdentity: bss.BSSID.String()}
		if includeSSID {
			wireless.SSID = bss.SSID
		}
		return wireless, true
	}
	return linuxWireless{}, false
}

func wifiChannel(frequency int) int {
	switch {
	case frequency == 2484:
		return 14
	case frequency >= 2412 && frequency <= 2472:
		return (frequency - 2407) / 5
	case frequency >= 5000 && frequency <= 5895:
		return (frequency - 5000) / 5
	case frequency >= 5955 && frequency <= 7115:
		return (frequency - 5950) / 5
	default:
		return 0
	}
}

func linuxDefaultRoute() (string, string) {
	file, err := os.Open("/proc/net/route")
	if err != nil {
		return "", ""
	}
	defer func() { _ = file.Close() }() // read-only; close error is not meaningful
	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		return "", ""
	}
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 4 && fields[1] == "00000000" {
			raw := fields[2]
			if len(raw) != 8 {
				continue
			}
			parts := []string{raw[6:8], raw[4:6], raw[2:4], raw[0:2]}
			bytes := make([]string, 4)
			for index, part := range parts {
				value, err := strconv.ParseUint(part, 16, 8)
				if err != nil {
					return fields[0], ""
				}
				bytes[index] = strconv.FormatUint(value, 10)
			}
			return fields[0], strings.Join(bytes, ".")
		}
	}
	return "", ""
}

func linuxWirelessRSSI(iface string) (float64, bool) {
	file, err := os.Open("/proc/net/wireless")
	if err != nil {
		return 0, false
	}
	defer func() { _ = file.Close() }() // read-only; close error is not meaningful
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		name, values, ok := strings.Cut(scanner.Text(), ":")
		if !ok || strings.TrimSpace(name) != iface {
			continue
		}
		fields := strings.Fields(values)
		if len(fields) < 3 {
			return 0, false
		}
		rssi, err := strconv.ParseFloat(strings.TrimSuffix(fields[2], "."), 64)
		return rssi, err == nil
	}
	return 0, false
}

func linuxBattery() (bool, float64, bool) {
	entries, _ := filepath.Glob("/sys/class/power_supply/BAT*/capacity")
	if len(entries) == 0 {
		// No battery at all means mains power: the state is known, not
		// unreadable, so scheduled saturation tests are not deferred on
		// servers and single-board computers.
		return false, 0, true
	}
	raw, err := os.ReadFile(entries[0])
	if err != nil {
		return false, 0, false
	}
	pct, err := strconv.ParseFloat(strings.TrimSpace(string(raw)), 64)
	if err != nil {
		return false, 0, false
	}
	onBattery := true
	if mains, _ := filepath.Glob("/sys/class/power_supply/A*/online"); len(mains) > 0 {
		if online, err := os.ReadFile(mains[0]); err == nil {
			onBattery = strings.TrimSpace(string(online)) != "1"
		}
	}
	return onBattery, pct, true
}
