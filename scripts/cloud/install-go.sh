#!/usr/bin/env bash
# whatsmeow-only install: Go toolchain pinned by go.mod, then go build/test.
# Called by install.sh when this checkout has no auraRN/aura-hono-api siblings,
# and by install-workspace.sh as this repo's step in the multi-repo Build.
#
# No secrets. mobile/build-ios.sh is macOS-only and is not run here.
set -euo pipefail

cd "$(dirname "$0")/../.."

PINNED_GO="$(sed -n 's/^toolchain go//p' go.mod | tr -d '[:space:]')"
if [[ -z "$PINNED_GO" ]]; then
  PINNED_GO="$(sed -n 's/^go //p' go.mod | head -n1 | tr -d '[:space:]')"
fi
if [[ -z "$PINNED_GO" ]]; then
  echo "[cloud-install] Could not read Go version from go.mod." >&2
  exit 1
fi

CLOUD_DEFAULT_PATH="${HOME}/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
GO_HOME="${HOME}/.local/go"

go_os() {
  case "$(uname -s)" in
    Linux) echo linux ;;
    Darwin) echo darwin ;;
    *)
      echo "[cloud-install] unsupported OS $(uname -s)." >&2
      return 1
      ;;
  esac
}

go_arch() {
  case "$(uname -m)" in
    x86_64) echo amd64 ;;
    aarch64 | arm64) echo arm64 ;;
    *)
      echo "[cloud-install] unsupported arch $(uname -m)." >&2
      return 1
      ;;
  esac
}

current_go_version() {
  local bin="$1"
  if [[ ! -x "$bin" ]]; then
    return 1
  fi
  "$bin" version 2>/dev/null | awk '{print $3}' | sed 's/^go//'
}

persist_go_onto_cloud_path() {
  local dest="$1"
  shift
  "$@" mkdir -p "$dest"
  local name
  for name in go gofmt; do
    if [[ -e "$GO_HOME/bin/$name" ]]; then
      "$@" ln -sfn "$GO_HOME/bin/$name" "$dest/$name"
    fi
  done
}

# --- Go toolchain, pinned by go.mod `toolchain` -------------------------------
if [[ "$(current_go_version "${GO_HOME}/bin/go" || true)" != "$PINNED_GO" ]]; then
  echo "[cloud-install] Installing Go ${PINNED_GO} to $GO_HOME."
  tarball="go${PINNED_GO}.$(go_os)-$(go_arch).tar.gz"
  tmp="$(mktemp -d)"
  curl -fsSL "https://go.dev/dl/${tarball}" -o "$tmp/go.tar.gz"
  rm -rf "$GO_HOME"
  mkdir -p "$(dirname "$GO_HOME")"
  tar -C "$(dirname "$GO_HOME")" -xzf "$tmp/go.tar.gz"
  if [[ ! -x "$GO_HOME/bin/go" && -x "$(dirname "$GO_HOME")/go/bin/go" ]]; then
    mv "$(dirname "$GO_HOME")/go" "$GO_HOME"
  fi
  rm -rf "$tmp"
fi

if [[ ! -x "$GO_HOME/bin/go" ]]; then
  echo "[cloud-install] go binary missing at $GO_HOME/bin/go." >&2
  exit 1
fi
if [[ "$(current_go_version "$GO_HOME/bin/go")" != "$PINNED_GO" ]]; then
  echo "[cloud-install] installed go is $(current_go_version "$GO_HOME/bin/go"), expected $PINNED_GO." >&2
  exit 1
fi

export PATH="$GO_HOME/bin:$PATH"
export GOTOOLCHAIN=local
export GOPATH="${GOPATH:-$HOME/go}"

persist_go_onto_cloud_path "${HOME}/.local/bin"
if sudo -n true >/dev/null 2>&1; then
  persist_go_onto_cloud_path /usr/local/bin sudo -n
fi

if ! grep -qs '\.local/go/bin' "$HOME/.bashrc" 2>/dev/null; then
  {
    echo ''
    echo '# Go pinned by whatsmeow go.mod toolchain (scripts/cloud/install-go.sh).'
    echo 'export PATH="$HOME/.local/go/bin:$PATH"'
    echo 'export GOTOOLCHAIN=local'
  } >> "$HOME/.bashrc"
fi

if ! CLEAN_GO_VERSION="$(env -i HOME="$HOME" PATH="$CLOUD_DEFAULT_PATH" bash --noprofile --norc -c 'go version')" \
  || ! grep -Fq "go$PINNED_GO" <<<"$CLEAN_GO_VERSION"; then
  echo "[cloud-install] go is not the pinned version on Cursor Cloud's reset default PATH (got '${CLEAN_GO_VERSION:-missing}', expected go$PINNED_GO)." >&2
  exit 1
fi

go mod download
go build ./...
go test ./...

echo "[cloud-install] whatsmeow Go install done."
