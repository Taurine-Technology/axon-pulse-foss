#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
  echo "Managed Axon Pulse removal requires administrator privileges." >&2
  exit 1
fi
launchctl bootout system/com.taurine.axon-pulse.managed 2>/dev/null || true
rm -f /Library/LaunchDaemons/com.taurine.axon-pulse.managed.plist
rm -f /usr/local/bin/axon-pulse
rm -rf "/Library/Application Support/Axon Pulse" /var/run/axon-pulse
echo "Managed Axon Pulse and its local credentials/history were removed."
