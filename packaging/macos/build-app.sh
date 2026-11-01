#!/bin/sh
# build-app.sh — assemble Ether.app (menu-bar tray + server + ffmpeg).
#
# Layout (design doc §Packaging):
#   Ether.app/Contents/Info.plist            (LSUIElement menu-bar app)
#   Ether.app/Contents/MacOS/ether-tray       (the app; CGO systray build)
#   Ether.app/Contents/Resources/ether        (server, universal CGO_FREE)
#   Ether.app/Contents/Resources/ffmpeg, ffprobe (universal, via lipo)
#   Ether.app/Contents/Resources/io.github.braedonsaunders.ether.plist (server agent template)
#   Ether.app/Contents/Resources/Ether.icns   (if present in packaging/macos/)
#
# Usage: VERSION=1.2.0 [SIGN_IDENTITY="Developer ID Application: ..."] packaging/macos/build-app.sh [outdir]
# Signing/notarisation happen only when SIGN_IDENTITY is set (CI). Unsigned
# local builds run with a right-click > Open the first time.
set -eu

# Clang otherwise targets the build host's macOS version. Keep the GUI
# launcher compatible with the minimum advertised by Info.plist.
export MACOSX_DEPLOYMENT_TARGET=13.0
# Explicit flags also invalidate CGO objects cached for a newer deployment
# target; MACOSX_DEPLOYMENT_TARGET alone is not part of Go's build-cache key.
export CGO_CFLAGS="${CGO_CFLAGS:-} -mmacosx-version-min=13.0"
export CGO_CXXFLAGS="${CGO_CXXFLAGS:-} -mmacosx-version-min=13.0"
export CGO_LDFLAGS="${CGO_LDFLAGS:-} -mmacosx-version-min=13.0"

# shellcheck disable=SC1007
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
VERSION=${VERSION:-dev}
BUILD=${BUILD:-1}
OUT=${1:-"$ROOT/dist/macos"}
APP="$OUT/Ether.app"
CONTENTS="$APP/Contents"
MACOS="$CONTENTS/MacOS"
RES="$CONTENTS/Resources"

cd "$ROOT"
mkdir -p "$MACOS" "$RES" "$ROOT/dist/tmp-mac"

LDFLAGS="-s -w \
  -X github.com/braedonsaunders/ether/internal/updater.Version=$VERSION \
  -X github.com/braedonsaunders/ether/internal/updater.Commit=${COMMIT:-} \
  -X github.com/braedonsaunders/ether/internal/updater.PublicKeyB64=${ETHER_UPDATE_PUBLIC_KEY:-} \
  -X github.com/braedonsaunders/ether/internal/tmdb.BundledKey=${ETHER_TMDB_BUNDLED_KEY:-}"

echo "==> building server (universal, CGO_ENABLED=0)"
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=darwin GOARCH=$arch go build -trimpath -ldflags "$LDFLAGS" \
    -o "dist/tmp-mac/ether-$arch" ./cmd/ether
done
lipo -create -output "$RES/ether" dist/tmp-mac/ether-amd64 dist/tmp-mac/ether-arm64

echo "==> building tray (universal, CGO_ENABLED=1 — needs a Mac)"
for arch in amd64 arm64; do
  goarch=$arch
  if [ "$arch" = "amd64" ]; then ccarch=x86_64; else ccarch=arm64; fi
  CGO_ENABLED=1 GOOS=darwin GOARCH=$goarch \
    CC="clang -arch $ccarch" \
    go build -trimpath -ldflags "$LDFLAGS" -o "dist/tmp-mac/ether-tray-$arch" ./cmd/ether-tray
done
lipo -create -output "$MACOS/ether-tray" dist/tmp-mac/ether-tray-amd64 dist/tmp-mac/ether-tray-arm64

echo "==> staging ffmpeg (universal)"
scripts/fetch-ffmpeg.sh darwin-amd64 darwin-arm64
for tool in ffmpeg ffprobe; do
  lipo -create -output "$RES/$tool" \
    "dist/ffmpeg/darwin-amd64/$tool" "dist/ffmpeg/darwin-arm64/$tool"
  chmod +x "$RES/$tool"
done

echo "==> bundle metadata"
sed -e "s/__VERSION__/$VERSION/" -e "s/__BUILD__/$BUILD/" \
  packaging/macos/Info.plist > "$CONTENTS/Info.plist"
cp packaging/macos/io.github.braedonsaunders.ether.plist "$RES/"
cp LICENSE THIRD_PARTY_NOTICES.md "$RES/"
[ -f packaging/macos/Ether.icns ] && cp packaging/macos/Ether.icns "$RES/"
chmod +x "$MACOS/ether-tray" "$RES/ether"
rm -rf dist/tmp-mac

python3 - "$MACOS/ether-tray" "$RES/ether" "$RES/ffmpeg" "$RES/ffprobe" <<'PY'
import re, subprocess, sys
for binary in sys.argv[1:]:
    commands = subprocess.check_output(["otool", "-l", binary], text=True)
    versions = re.findall(r"\bminos\s+([\d.]+)", commands)
    if not versions:
        raise SystemExit(f"Missing minimum OS metadata: {binary}")
    for version in versions:
        if tuple(map(int, version.split(".")[:2])) > (13, 0):
            raise SystemExit(f"{binary} requires macOS {version}; bundle advertises 13.0")
print("All bundled architectures have a macOS deployment target <= 13.0")
PY

if [ -n "${SIGN_IDENTITY:-}" ]; then
  echo "==> codesigning ($SIGN_IDENTITY)"
  # Sign nested binaries first, then the bundle. Hardened runtime +
  # timestamp are required for notarisation.
  for bin in "$RES/ether" "$RES/ffmpeg" "$RES/ffprobe" "$MACOS/ether-tray"; do
    codesign --force --options runtime --timestamp --sign "$SIGN_IDENTITY" "$bin"
  done
  codesign --force --options runtime --timestamp --sign "$SIGN_IDENTITY" "$APP"
  codesign --verify --deep --strict "$APP"
else
  # lipo invalidates input signatures. Apple Silicon needs a valid ad-hoc
  # signature even when no Developer ID is available for distribution.
  echo "==> SIGN_IDENTITY unset: ad-hoc signing (not notarised)"
  for bin in "$RES/ether" "$RES/ffmpeg" "$RES/ffprobe" "$MACOS/ether-tray"; do
    codesign --force --sign - "$bin"
  done
  codesign --force --sign - "$APP"
  codesign --verify --deep --strict "$APP"
fi

echo "==> built $APP"
