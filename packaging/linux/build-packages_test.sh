#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
test_root=$(mktemp -d "${TMPDIR:-/tmp}/axon-pulse-packaging-test.XXXXXX")
trap 'rm -rf -- "$test_root"' 0 HUP INT TERM

if VERSION='1.2/3' OUTPUT_DIR="$test_root/invalid" \
  PULSE_DESKTOP_PACKAGE_BINARY=/bin/true \
  "$script_dir/build-packages.sh" >/dev/null 2>&1; then
  echo "unsafe VERSION unexpectedly accepted" >&2
  exit 1
fi

output="$test_root/out"
VERSION=1.2.3-preview.8 ARCH=amd64 OUTPUT_DIR="$output" \
  PULSE_DESKTOP_PACKAGE_BINARY=/bin/true \
  "$script_dir/build-packages.sh" >/dev/null
package="$output/axon-pulse-desktop_1.2.3-preview.8_linux_amd64.deb"
test -f "$package"

# A leftover tree from the legacy in-output staging layout must not enter a
# subsequent package built into the same output directory.
mkdir -p "$output/desktop/usr/lib/axon-pulse"
install -m 0644 /dev/null "$output/desktop/usr/lib/axon-pulse/stale"
VERSION=1.2.3-preview.8 ARCH=amd64 OUTPUT_DIR="$output" \
  PULSE_DESKTOP_PACKAGE_BINARY=/bin/true \
  "$script_dir/build-packages.sh" >/dev/null
if dpkg-deb --contents "$package" | grep -q '/stale$'; then
  echo "stale staging file entered rebuilt package" >&2
  exit 1
fi

# Epoch migrates legacy previews; ~ orders prereleases before the final version.
test "$(dpkg-deb -f "$package" Version)" = '1:1.2.3~preview.8'
dpkg --compare-versions '1:0.0.8~alpha.1' gt '0.1.0-preview.10'
dpkg --compare-versions '1:1.2.3~preview.8' lt '1:1.2.3'
