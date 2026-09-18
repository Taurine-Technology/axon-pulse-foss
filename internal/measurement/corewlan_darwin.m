// CoreWLAN bridge for the darwin link collector. Compiled only for cgo
// builds; the static CLI falls back to the system_profiler parser.
#import <CoreWLAN/CoreWLAN.h>
#import <Foundation/Foundation.h>
#include <string.h>

#include "corewlan_darwin.h"

static void copyString(NSString *value, char *out, size_t capacity) {
	out[0] = '\0';
	if (value == nil) {
		return;
	}
	const char *utf8 = [value UTF8String];
	if (utf8 == NULL) {
		return;
	}
	strlcpy(out, utf8, capacity);
}

int axon_corewlan_info(AxonWifiInfo *info) {
	memset(info, 0, sizeof(*info));
	@autoreleasepool {
		CWWiFiClient *client = [CWWiFiClient sharedWiFiClient];
		CWInterface *interface = [client interface];
		if (interface == nil || ![interface powerOn]) {
			return 1;
		}
		info->hasInterface = 1;
		info->rssi = (long)[interface rssiValue];
		info->noise = (long)[interface noiseMeasurement];
		info->txRate = [interface transmitRate];
		info->phyMode = (long)[interface activePHYMode];
		info->security = (long)[interface security];
		CWChannel *channel = [interface wlanChannel];
		if (channel != nil) {
			info->channel = (long)[channel channelNumber];
			info->band = (long)[channel channelBand];
			info->width = (long)[channel channelWidth];
		}
		// SSID and BSSID come back nil without Location Services; every other
		// field above is exempt from that gate.
		copyString([interface ssid], info->ssid, sizeof(info->ssid));
		copyString([interface bssid], info->bssid, sizeof(info->bssid));
		info->associated = (channel != nil && info->rssi < 0) ? 1 : 0;
		return 0;
	}
}
