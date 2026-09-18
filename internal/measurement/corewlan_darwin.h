#ifndef AXON_COREWLAN_DARWIN_H
#define AXON_COREWLAN_DARWIN_H

// Snapshot of the default Wi-Fi interface via CoreWLAN. Numeric fields are 0
// when unavailable; ssid/bssid are empty unless the process holds Location
// Services authorization (macOS redacts them without it, by design).
typedef struct {
	int hasInterface;   // a Wi-Fi interface exists and is powered on
	int associated;     // the interface is joined to a network
	long rssi;          // dBm, negative when valid
	long noise;         // dBm, negative when valid
	double txRate;      // Mbps
	long channel;       // channel number
	long band;          // CWChannelBand enum value
	long width;         // CWChannelWidth enum value
	long phyMode;       // CWPHYMode enum value
	long security;      // CWSecurity enum value
	char ssid[64];      // NUL-terminated, empty when redacted
	char bssid[64];     // NUL-terminated, empty when redacted
} AxonWifiInfo;

// Returns 0 on success (info filled), non-zero when CoreWLAN reports no
// usable Wi-Fi interface.
int axon_corewlan_info(AxonWifiInfo *info);

#endif
