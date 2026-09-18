#!/bin/sh
set -eu

launchctl bootout "gui/$(id -u)/com.taurine.axon-pulse" 2>/dev/null || true
rm -f "$HOME/Library/LaunchAgents/com.taurine.axon-pulse.plist"
rm -rf "$HOME/Library/Application Support/axon-pulse" "$HOME/Library/Application Support/Axon Pulse"
rm -rf "/Applications/Axon Pulse.app"
echo "Axon Pulse and its local credentials/history were removed."
