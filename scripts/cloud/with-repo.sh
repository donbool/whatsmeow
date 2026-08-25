#!/usr/bin/env bash
# Run a command inside one sibling with that repo's pinned toolchain on PATH.
# Usage (from the whatsmeow repo root):
#   bash scripts/cloud/with-repo.sh auraRN bun run test:ci
#   bash scripts/cloud/with-repo.sh aura-hono-api bun run verify:env
#   bash scripts/cloud/with-repo.sh whatsmeow go test ./...
set -euo pipefail

cd "$(dirname "$0")/../.."
# shellcheck source=workspace.sh
. "$(dirname "$0")/workspace.sh"

if [[ $# -lt 2 ]]; then
  echo "usage: $0 <auraRN|aura-hono-api|whatsmeow> <command> [args...]" >&2
  exit 2
fi

repo="$1"
shift
dest="$(authintel_repo_path "$repo")"
if [[ ! -d "$dest" ]]; then
  echo "[with-repo] missing checkout $repo at $dest" >&2
  exit 1
fi

case "$repo" in
  auraRN)
    pin="$(authintel_read_bun_pin "$dest")"
    export BUN_INSTALL="${HOME}/.bun-versions/${pin}"
    export PATH="${BUN_INSTALL}/bin:${PATH}"
    ;;
  aura-hono-api)
    export BUN_INSTALL="${HOME}/.bun"
    export PATH="${BUN_INSTALL}/bin:${PATH}"
    ;;
  whatsmeow)
    export PATH="${HOME}/.local/go/bin:${HOME}/.local/bin:${PATH}"
    ;;
  *)
    echo "[with-repo] unknown repo: $repo" >&2
    exit 2
    ;;
esac

cd "$dest"
exec "$@"
