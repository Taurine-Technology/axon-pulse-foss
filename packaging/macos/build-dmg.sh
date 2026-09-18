#!/bin/sh
set -eu

version=${VERSION:?VERSION is required}
build=${BUILD_NUMBER:-1}
binary=${PULSE_DESKTOP_BINARY:?PULSE_DESKTOP_BINARY is required}
output=${OUTPUT_DIR:-dist}
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
app="$output/Axon Pulse.app"

# Apple bundle versions cannot contain SemVer prerelease/build metadata.
# The full version remains embedded in the binary and release index.
bundle_version=${version%%[-+]*}
if ! printf '%s\n' "$bundle_version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
  echo "VERSION must start with a numeric major.minor.patch version" >&2
  exit 1
fi
if ! printf '%s\n' "$build" | grep -Eq '^[0-9]+(\.[0-9]+){0,2}$'; then
  echo "BUILD_NUMBER must contain one to three dot-separated integers" >&2
  exit 1
fi

mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
sed -e "s/__VERSION__/$bundle_version/g" -e "s/__BUILD__/$build/g" "$script_dir/Info.plist" > "$app/Contents/Info.plist"
install -m 0755 "$binary" "$app/Contents/MacOS/Axon Pulse"
install -m 0644 "$script_dir/../assets/axon-pulse.icns" "$app/Contents/Resources/axon-pulse.icns"

if [ -n "${APPLE_SIGNING_IDENTITY:-}" ]; then
  codesign --force --options runtime --timestamp --sign "$APPLE_SIGNING_IDENTITY" "$app"
  codesign --verify --deep --strict --verbose=2 "$app"
fi

notary_submit() {
  if [ -n "${APPLE_NOTARY_KEY:-}" ] && [ -n "${APPLE_NOTARY_KEY_ID:-}" ] && [ -n "${APPLE_NOTARY_ISSUER:-}" ]; then
    xcrun notarytool submit "$1" --key "$APPLE_NOTARY_KEY" --key-id "$APPLE_NOTARY_KEY_ID" --issuer "$APPLE_NOTARY_ISSUER" --wait
  elif [ -n "${APPLE_NOTARY_PROFILE:-}" ]; then
    xcrun notarytool submit "$1" --keychain-profile "$APPLE_NOTARY_PROFILE" --wait
  else
    return 1
  fi
}

# Notarize and staple the app before placing it in the image so offline
# Gatekeeper verification does not depend solely on the DMG's ticket.
notary_zip="$output/axon-pulse-macos-notary.zip"
if [ -n "${APPLE_NOTARY_KEY:-}${APPLE_NOTARY_PROFILE:-}" ]; then
  ditto -c -k --keepParent "$app" "$notary_zip"
  notary_submit "$notary_zip"
  xcrun stapler staple "$app"
  rm -f "$notary_zip"
fi

dmg_root="$output/axon-pulse-dmg-root"
rm -rf "$dmg_root"
mkdir -p "$dmg_root"
ditto "$app" "$dmg_root/Axon Pulse.app"
ln -s /Applications "$dmg_root/Applications"
dmg="$output/axon-pulse_macos_universal.dmg"
hdiutil create -volname "Axon Pulse" -srcfolder "$dmg_root" -ov -format UDZO "$dmg"
rm -rf "$dmg_root"

if [ -n "${APPLE_SIGNING_IDENTITY:-}" ]; then
  codesign --force --timestamp --sign "$APPLE_SIGNING_IDENTITY" "$dmg"
fi
if [ -n "${APPLE_NOTARY_KEY:-}${APPLE_NOTARY_PROFILE:-}" ]; then
  notary_submit "$dmg"
  xcrun stapler staple "$dmg"
fi
