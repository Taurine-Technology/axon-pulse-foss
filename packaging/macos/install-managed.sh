#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
  echo "Managed Axon Pulse installation requires administrator privileges." >&2
  exit 1
fi
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
binary=${AXON_PULSE_BINARY:-"$script_dir/axon-pulse"}
install -m 0755 "$binary" /usr/local/bin/axon-pulse
install -d -m 0700 "/Library/Application Support/Axon Pulse"
install -m 0644 "$script_dir/com.taurine.axon-pulse.managed.plist" /Library/LaunchDaemons/com.taurine.axon-pulse.managed.plist
launchctl bootout system/com.taurine.axon-pulse.managed 2>/dev/null || true
launchctl bootstrap system /Library/LaunchDaemons/com.taurine.axon-pulse.managed.plist
echo "Managed Axon Pulse is installed and runs before login."
