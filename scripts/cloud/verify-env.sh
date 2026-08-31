#!/usr/bin/env bash
# Workspace-level environment verification after a multi-repo Build.
#
# Each sibling install already ran its own verify when secrets were present.
# This only re-asserts cross-repo PATH invariants after Cloud's PATH reset —
# no second prod Gel hit.
set -euo pipefail

cd "$(dirname "$0")/../.."
# shellcheck source=workspace.sh
. "$(dirname "$0")/workspace.sh"

fail() {
  echo "[verify-env] FAILED: $*" >&2
  exit 1
}

log() {
  echo "[verify-env] $*"
}

for repo in "${AUTHINTEL_REPOS[@]}"; do
  dest="$(authintel_repo_path "$repo")"
  if [[ ! -d "$dest" ]]; then
    fail "missing checkout $repo at $dest"
  fi
  log "OK checkout: $repo"
done

AURARN="$(authintel_repo_path auraRN)"
HONO="$(authintel_repo_path aura-hono-api)"
[[ -d "$AURARN" && -d "$HONO" ]] || fail "megpt-mono is missing auraRN/ or aura-hono-api/."

AURARN_BUN="$(authintel_read_bun_pin "$AURARN")"
HONO_BUN="$(authintel_read_bun_pin "$HONO")"
GO_PIN="$(authintel_read_go_toolchain "$WHATSMEOW_ROOT")"
[[ -n "$AURARN_BUN" && -n "$HONO_BUN" && -n "$GO_PIN" ]] || fail "could not read tool pins from megpt-mono and whatsmeow."
if [[ "$AURARN_BUN" != "$HONO_BUN" ]]; then
  fail "Bun pins diverge inside megpt-mono: auraRN=$AURARN_BUN aura-hono-api=$HONO_BUN."
fi
MEGPT_BUN="$HONO_BUN"

clean_bun="$(authintel_clean_cmd 'bun --version' || true)"
if [[ "$clean_bun" != "$MEGPT_BUN" ]]; then
  fail "bun on Cursor Cloud's reset default PATH is '${clean_bun:-missing}', expected megpt-mono pin '$MEGPT_BUN'."
fi
log "OK default bun: $clean_bun"

clean_go="$(authintel_clean_cmd 'go version' || true)"
if ! grep -Fq "go$GO_PIN" <<<"$clean_go"; then
  fail "go on Cursor Cloud's reset default PATH is '${clean_go:-missing}', expected toolchain go$GO_PIN."
fi
log "OK go: $clean_go"

MEGPT="$(authintel_repo_path megpt-mono)"
if ! authintel_clean_cmd 'redis-cli ping' >/dev/null 2>&1; then
  log "Redis not responding; running megpt-mono start.sh."
  bash "$MEGPT/scripts/cloud/start.sh"
fi
if ! redis-cli ping >/dev/null 2>&1; then
  fail "Redis is not up on :6379."
fi
log "OK Redis on :6379"

log "Workspace environment OK."
