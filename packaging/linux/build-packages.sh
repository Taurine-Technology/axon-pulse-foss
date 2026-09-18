#!/bin/sh
set -eu

version=${VERSION:?VERSION is required}
arch=${ARCH:-amd64}
headless=${PULSE_HEADLESS_BINARY:-}
headless_package=${PULSE_HEADLESS_PACKAGE_BINARY:-}
desktop=${PULSE_DESKTOP_BINARY:-}
desktop_package=${PULSE_DESKTOP_PACKAGE_BINARY:-${PULSE_DESKTOP_EXTERNAL_BINARY:-}}
output=${OUTPUT_DIR:-dist}
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

if ! printf '%s\n' "$version" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'; then
  echo "VERSION must be a SemVer value safe for Debian packaging: $version" >&2
  exit 1
fi

# Debian uses ~ so a prerelease sorts before the corresponding final release.
# Epoch 1 also migrates legacy 0.1.0-preview packages that predate 0.0.x releases.
deb_version="1:$(printf '%s' "$version" | sed 's/^\([0-9]*\.[0-9]*\.[0-9]*\)-/\1~/')"
case "$arch" in amd64|arm64) ;; *) echo "unsupported architecture: $arch" >&2; exit 1 ;; esac

mkdir -p "$output"
output=$(CDPATH= cd -- "$output" && pwd)
repository=$(CDPATH= cd -- "$script_dir/../.." && pwd)
if [ "$output" = / ] || [ "$output" = "$repository" ]; then
  echo "refusing unsafe OUTPUT_DIR: $output" >&2
  exit 1
fi
rm -rf -- "$output/core" "$output/AppDir" "$output/desktop"
find "$output" -maxdepth 1 -type f \( \
  -name 'axon-pulse_*.deb' -o \
  -name 'axon-pulse-desktop_*.deb' -o \
  -name 'axon-pulse_*.tar.gz' -o \
  -name 'axon-pulse_*.AppImage' \
\) -delete
staging=$(mktemp -d "${TMPDIR:-/tmp}/axon-pulse-linux.XXXXXX")
trap 'rm -rf -- "$staging"' 0 HUP INT TERM

if [ -n "$headless" ]; then
  : "${headless_package:?PULSE_HEADLESS_PACKAGE_BINARY must be an APT-managed build}"
  core="$staging/core"
  mkdir -p "$core/usr/bin" "$core/lib/systemd/system" "$core/DEBIAN"
  install -m 0755 "$headless_package" "$core/usr/bin/axon-pulse"
  install -m 0644 "$script_dir/axon-pulse.service" "$core/lib/systemd/system/axon-pulse.service"
  sed "s/__VERSION__/$deb_version/;s/__ARCH__/$arch/" "$script_dir/control.core" > "$core/DEBIAN/control"
  install -m 0755 "$script_dir/postinst.core" "$core/DEBIAN/postinst"
  install -m 0755 "$script_dir/prerm.core" "$core/DEBIAN/prerm"
  install -m 0755 "$script_dir/postrm.core" "$core/DEBIAN/postrm"
  dpkg-deb --build --root-owner-group "$core" "$output/axon-pulse_${version}_linux_${arch}.deb"

  tar -C "$(dirname "$headless")" -czf "$output/axon-pulse_${version}_linux_${arch}.tar.gz" "$(basename "$headless")"
fi

if [ -n "$desktop" ]; then
  appdir="$staging/AppDir"
  mkdir -p "$appdir/usr/bin" "$appdir/usr/share/applications" "$appdir/usr/share/icons/hicolor/scalable/apps"
  install -m 0755 "$desktop" "$appdir/usr/bin/axon-pulse-desktop"
  install -m 0755 "$script_dir/AppRun" "$appdir/AppRun"
  install -m 0644 "$script_dir/axon-pulse.desktop" "$appdir/axon-pulse.desktop"
  install -m 0644 "$script_dir/../assets/axon-pulse.svg" "$appdir/axon-pulse.svg"
  install -m 0644 "$script_dir/axon-pulse.desktop" "$appdir/usr/share/applications/axon-pulse.desktop"
  install -m 0644 "$script_dir/../assets/axon-pulse.svg" "$appdir/usr/share/icons/hicolor/scalable/apps/axon-pulse.svg"
  if command -v appimagetool >/dev/null 2>&1; then
    ARCH="$arch" appimagetool "$appdir" "$output/axon-pulse_linux_${arch}.AppImage"
  fi
fi

if [ -n "$desktop_package" ]; then
  desktop_root="$staging/desktop"
  mkdir -p "$desktop_root/etc/apparmor.d" "$desktop_root/usr/bin" "$desktop_root/usr/lib/axon-pulse" "$desktop_root/usr/share/applications" "$desktop_root/usr/share/icons/hicolor/scalable/apps" "$desktop_root/DEBIAN"
  install -m 0755 "$desktop_package" "$desktop_root/usr/lib/axon-pulse/axon-pulse-desktop"
  install -m 0755 "$script_dir/desktop-wrapper.sh" "$desktop_root/usr/bin/axon-pulse-desktop"
  install -m 0644 "$script_dir/axon-pulse-desktop.apparmor" "$desktop_root/etc/apparmor.d/axon-pulse-desktop"
  install -m 0644 "$script_dir/axon-pulse.desktop" "$desktop_root/usr/share/applications/axon-pulse.desktop"
  install -m 0644 "$script_dir/../assets/axon-pulse.svg" "$desktop_root/usr/share/icons/hicolor/scalable/apps/axon-pulse.svg"
  sed "s/__VERSION__/$deb_version/;s/__ARCH__/$arch/" "$script_dir/control.desktop" > "$desktop_root/DEBIAN/control"
  install -m 0755 "$script_dir/postinst.desktop" "$desktop_root/DEBIAN/postinst"
  install -m 0755 "$script_dir/prerm.desktop" "$desktop_root/DEBIAN/prerm"
  dpkg-deb --build --root-owner-group "$desktop_root" "$output/axon-pulse-desktop_${version}_linux_${arch}.deb"
fi
