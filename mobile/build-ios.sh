#!/usr/bin/env bash
# Build a minimal iOS xcframework from the gomobile wrapper package.
#
# Usage: ./mobile/build-ios.sh
# Optional:
#   ALLOW_DIRTY=1       permit a build from a dirty git checkout
#   SOURCE_COMMIT=<sha> identify an exported source tree without .git metadata
#   WA_IOS_TARGETS=...  override the default device + Apple-silicon simulator targets
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)" # whatsmeow repo root (has go.mod)
OUT="$ROOT/mobile/build/Wa.xcframework"
BUILD_COPY="/tmp/wa-build/whatsmeow"
TOOLS_DIR="$ROOT/mobile/build/tools"
GO_TOOLCHAIN="go1.26.4"
GOMOBILE_VERSION="v0.0.0-20260611195102-4dd8f1dbf5d2"
TARGETS="${WA_IOS_TARGETS:-ios/arm64,iossimulator/arm64}"

if ! command -v go >/dev/null 2>&1; then
	echo "error: go is required" >&2
	exit 1
fi
if ! command -v ditto >/dev/null 2>&1 || ! command -v /usr/libexec/PlistBuddy >/dev/null 2>&1; then
	echo "error: this build must run on macOS (ditto and PlistBuddy are required)" >&2
	exit 1
fi

mkdir -p "$ROOT/mobile/build"

# Release builds should be traceable to one clean source commit. Exported source
# trees can provide SOURCE_COMMIT explicitly (local development may use unknown).
SOURCE_COMMIT="${SOURCE_COMMIT:-unknown}"
if git -C "$ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	SOURCE_COMMIT="$(git -C "$ROOT" rev-parse HEAD)"
	if [[ -n "$(git -C "$ROOT" status --porcelain --untracked-files=normal)" ]]; then
		if [[ "${ALLOW_DIRTY:-0}" != "1" ]]; then
			echo "error: refusing to build a release artifact from a dirty checkout" >&2
			echo "commit/stash first, or set ALLOW_DIRTY=1 for a local-only build" >&2
			exit 1
		fi
		SOURCE_COMMIT="${SOURCE_COMMIT}-dirty"
	fi
fi

# Install gomobile and gobind from the exact x/mobile version pinned in go.mod.
# This never mutates go.mod/go.sum and avoids a machine-global @latest tool.
PINNED_GOBIN="$TOOLS_DIR/$GOMOBILE_VERSION"
mkdir -p "$PINNED_GOBIN"
if [[ ! -x "$PINNED_GOBIN/gomobile" || ! -x "$PINNED_GOBIN/gobind" ]]; then
	echo "Installing pinned gomobile toolchain: $GOMOBILE_VERSION"
	GOTOOLCHAIN="$GO_TOOLCHAIN" GOBIN="$PINNED_GOBIN" \
		go install "golang.org/x/mobile/cmd/gomobile@$GOMOBILE_VERSION"
	GOTOOLCHAIN="$GO_TOOLCHAIN" GOBIN="$PINNED_GOBIN" \
		go install "golang.org/x/mobile/cmd/gobind@$GOMOBILE_VERSION"
fi
export PATH="$PINNED_GOBIN:$PATH"
GOTOOLCHAIN="$GO_TOOLCHAIN" "$PINNED_GOBIN/gomobile" init

# Build from a neutral path. Go embeds the main module directory in build info,
# and -trimpath alone does not scrub every occurrence. Exclude build artifacts
# and git internals so the copy is bounded and cannot recursively copy itself.
mkdir -p "$(dirname "$BUILD_COPY")"
rm -rf "$BUILD_COPY"
mkdir -p "$BUILD_COPY"
tar -C "$ROOT" --exclude=".git" --exclude="mobile/build" -cf - . |
	tar -C "$BUILD_COPY" -xf -
cd "$BUILD_COPY"

GOTOOLCHAIN="$GO_TOOLCHAIN" go mod download

# -trimpath drops per-file build paths from the binary.
# -ldflags "-s -w" strips the symbol table + DWARF (much smaller binary).
rm -rf "$OUT"
LDFLAGS="-s -w"
LDFLAGS+=" -X go.mau.fi/whatsmeow/mobile/wa.buildSourceCommit=$SOURCE_COMMIT"
LDFLAGS+=" -X go.mau.fi/whatsmeow/mobile/wa.buildGomobileVersion=$GOMOBILE_VERSION"
GOTOOLCHAIN="$GO_TOOLCHAIN" "$PINNED_GOBIN/gomobile" bind \
	-target="$TARGETS" \
	-trimpath \
	-ldflags="$LDFLAGS" \
	-o "$OUT" \
	./mobile/wa
echo "Built: $OUT"

# gomobile stamps MinimumOSVersion 100.0, which App Store archive validation
# rejects. Patch every generated slice, not only the physical-device framework.
PLISTS=("$OUT"/*/Wa.framework/Info.plist)
if [[ ! -e "${PLISTS[0]}" ]]; then
	echo "error: no generated Wa.framework Info.plist files found" >&2
	exit 1
fi
for plist in "${PLISTS[@]}"; do
	/usr/libexec/PlistBuddy -c "Set :MinimumOSVersion 18.0" "$plist"
done

# Release artifact: consuming apps download a pinned GitHub release instead of
# vendoring the framework. Publishing stays a deliberate manual step.
ZIP="$ROOT/mobile/build/Wa.xcframework.zip"
MANIFEST="$ROOT/mobile/build/Wa.xcframework.manifest.json"
rm -f "$ZIP"
ditto -c -k --keepParent "$OUT" "$ZIP"
SHA256="$(shasum -a 256 "$ZIP" | cut -d' ' -f1)"
echo "$SHA256" >"$ZIP.sha256"
cat >"$MANIFEST" <<EOF
{
  "sourceCommit": "$SOURCE_COMMIT",
  "goToolchain": "$GO_TOOLCHAIN",
  "gomobileVersion": "$GOMOBILE_VERSION",
  "targets": "$TARGETS",
  "minimumIOSVersion": "18.0",
  "sha256": "$SHA256"
}
EOF
echo ""
echo "Zipped:  $ZIP"
echo "SHA256:  $SHA256"
echo "Manifest: $MANIFEST"
echo "To publish (bump the tag number):"
echo "  gh release create wa-ios-vN \"$ZIP\" \"$ZIP.sha256\" \"$MANIFEST\" --verify-tag --repo donbool/whatsmeow --title \"Wa.xcframework vN\" --notes \"Built from $SOURCE_COMMIT with gomobile $GOMOBILE_VERSION\""
