#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
    echo "Axon Pulse headless installation must be run as root." >&2
    exit 1
fi

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
binary=${AXON_PULSE_BINARY:-"$script_dir/../../bin/axon-pulse"}

if [ ! -f "$binary" ]; then
    echo "Axon Pulse binary not found at $binary" >&2
    exit 1
fi

install -m 0755 "$binary" /usr/bin/axon-pulse
install -m 0644 "$script_dir/axon-pulse.service" /etc/systemd/system/axon-pulse.service
systemctl daemon-reload
systemctl enable --now axon-pulse.service

echo "Axon Pulse is installed and running."
echo "Connect it with: axon-pulse connect --url https://controller.example --token spt_..."
