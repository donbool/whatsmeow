#!/usr/bin/env bash
# Build a minimal iOS xcframework from the gomobile wrapper package.
#
# Prereqs (one-time):
#   go install golang.org/x/mobile/cmd/gomobile@latest
#   go install golang.org/x/mobile/cmd/gobind@latest
#   gomobile init
#
# Usage: ./mobile/build-ios.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)" # whatsmeow repo root (has go.mod)
GOBIN="$(go env GOPATH)/bin"
OUT="$ROOT/mobile/build/Wa.xcframework"
BUILD_COPY="/tmp/wa-build/whatsmeow"

# gomobile shells out to gobind; make sure both are reachable.
export PATH="$GOBIN:$PATH"

mkdir -p "$ROOT/mobile/build"

# Build from a neutral path: Go embeds the main module's directory in the
# binary's build info and -trimpath doesn't scrub it, so building in place
# would leak the local machine's paths into a published binary. The APFS
# clonefile copy (cp -c) is instant and space-free.
mkdir -p "$(dirname "$BUILD_COPY")"
rm -rf "$BUILD_COPY"
cp -Rc "$ROOT" "$BUILD_COPY"
cd "$BUILD_COPY"

# Ensure bind support + the pure-Go sqlite driver are resolvable in this module.
go get golang.org/x/mobile/bind >/dev/null 2>&1 || true
go get modernc.org/sqlite >/dev/null 2>&1 || true

# -trimpath drops per-file build paths from the binary.
# -ldflags "-s -w" strips the symbol table + DWARF (much smaller binary).
# Device-only target (ios/arm64): we develop and ship on physical devices, so
# the arm64 simulator slice is pure dead weight (~25MB). If you ever need the
# iOS Simulator, append `,iossimulator/arm64` to -target and rebuild.
"$GOBIN/gomobile" bind -target=ios/arm64,iossimulator/arm64 -trimpath -ldflags="-s -w" -o "$OUT" ./mobile/wa
echo "Built: $OUT"

# gomobile stamps MinimumOSVersion 100.0, which App Store archive validation
# rejects; pin it to the consuming app's deployment target.
/usr/libexec/PlistBuddy -c "Set :MinimumOSVersion 18.0" \
	"$OUT/ios-arm64/Wa.framework/Info.plist"

# Release artifact: consuming apps download a pinned GitHub release instead of
# vendoring the framework. Publishing stays a deliberate manual step.
ZIP="$ROOT/mobile/build/Wa.xcframework.zip"
rm -f "$ZIP"
ditto -c -k --keepParent "$OUT" "$ZIP"
SHA256="$(shasum -a 256 "$ZIP" | cut -d' ' -f1)"
echo "$SHA256" >"$ZIP.sha256"
COMMIT="$(git -C "$ROOT" rev-parse --short HEAD)"
echo ""
echo "Zipped:  $ZIP"
echo "SHA256:  $SHA256"
echo "To publish (bump the tag number):"
echo "  gh release create wa-ios-vN \"$ZIP\" --repo donbool/whatsmeow --title \"Wa.xcframework vN\" --notes \"Built from $COMMIT\""
