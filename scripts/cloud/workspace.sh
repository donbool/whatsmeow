#!/usr/bin/env bash
# Shared paths for the authintel multi-repo cloud environment.
# This file lives in whatsmeow (the repo that holds .cursor/environment.json).
# Cursor clones the two repos as siblings; workspace root is whatsmeow's parent:
#   <workspace>/megpt-mono/            ← MeGPT monorepo (auraRN + aura-hono-api)
#   <workspace>/whatsmeow/             ← this repo
#
# Cursor Cloud's reset default PATH:
#   $HOME/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

WHATSMEOW_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
AUTHINTEL_WORKSPACE="$(cd "$WHATSMEOW_ROOT/.." && pwd)"
CLOUD_DEFAULT_PATH="${HOME}/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

AUTHINTEL_REPOS=(megpt-mono whatsmeow)

authintel_repo_url() {
  case "$1" in
    megpt-mono) echo "https://github.com/Authentic-Intelligence/megpt-mono.git" ;;
    whatsmeow) echo "https://github.com/donbool/whatsmeow.git" ;;
    *)
      echo "unknown repo: $1" >&2
      return 1
      ;;
  esac
}

authintel_repo_path() {
  case "$1" in
    whatsmeow) echo "$WHATSMEOW_ROOT" ;;
    megpt-mono) echo "${AUTHINTEL_WORKSPACE}/megpt-mono" ;;
    auraRN) echo "${AUTHINTEL_WORKSPACE}/megpt-mono/auraRN" ;;
    aura-hono-api) echo "${AUTHINTEL_WORKSPACE}/megpt-mono/aura-hono-api" ;;
    *)
      echo "unknown repo: $1" >&2
      return 1
      ;;
  esac
}

# True when this checkout is sitting next to megpt-mono (the multi-repo Cloud
# layout, or a local authintel/ folder). A whatsmeow-only clone has no sibling
# and should not try to install it.
authintel_has_workspace_siblings() {
  [[ -d "$(authintel_repo_path megpt-mono)" ]]
}

authintel_read_bun_pin() {
  sed -n 's/.*"packageManager": *"bun@\([0-9A-Za-z.-]*\)".*/\1/p' "$1/package.json" | head -n 1
}

authintel_read_go_toolchain() {
  local pinned
  pinned="$(sed -n 's/^toolchain go//p' "$1/go.mod" | tr -d '[:space:]')"
  if [[ -z "$pinned" ]]; then
    pinned="$(sed -n 's/^go //p' "$1/go.mod" | head -n1 | tr -d '[:space:]')"
  fi
  echo "$pinned"
}

authintel_persist_into_dir() {
  local dest="$1"
  shift
  local src="$1"
  shift
  local name="$1"
  shift
  "$@" mkdir -p "$dest"
  if [[ -e "$src" ]]; then
    "$@" ln -sfn "$src" "$dest/$name"
  fi
}

authintel_persist_onto_cloud_path() {
  local src="$1"
  local name="$2"
  authintel_persist_into_dir "${HOME}/.local/bin" "$src" "$name"
  if sudo -n true >/dev/null 2>&1; then
    authintel_persist_into_dir /usr/local/bin "$src" "$name" sudo -n
  fi
}

authintel_clean_cmd() {
  env -i HOME="$HOME" PATH="$CLOUD_DEFAULT_PATH" bash --noprofile --norc -c "$*"
}
