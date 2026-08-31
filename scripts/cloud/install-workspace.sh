#!/usr/bin/env bash
# Authintel multi-repo install. Called by install.sh when megpt-mono is a
# sibling of this whatsmeow checkout.
#
# Idempotent. Delegates to each repo's own install:
#   megpt-mono  — both trees share one Bun pin; root scripts/cloud/install.sh
#                 runs aura-hono-api then auraRN (prod Gel/Instant secrets).
#   whatsmeow   — install-go.sh
#
# Builds do not inject user-scoped secrets. megpt-mono's install.sh fail-closes
# without AURADB_EDGEDB_DSN and INSTANT_APP_ADMIN_TOKEN, so when those are
# missing we bootstrap the monorepo from this repo instead of calling it.
set -euo pipefail

cd "$(dirname "$0")/../.."
# shellcheck source=workspace.sh
. "$(dirname "$0")/workspace.sh"

echo "[cloud-install] Workspace: $AUTHINTEL_WORKSPACE (whatsmeow at $WHATSMEOW_ROOT)"

clone_repo_if_missing() {
  local name="$1"
  local dest
  dest="$(authintel_repo_path "$name")"
  if [[ -d "$dest/.git" || -d "$dest/auraRN" || -f "$dest/package.json" || -f "$dest/go.mod" ]]; then
    echo "[cloud-install] $name already present at $dest."
    return 0
  fi
  local url
  url="$(authintel_repo_url "$name")"
  echo "[cloud-install] Cloning $name from $url."
  git clone "$url" "$dest"
}

# whatsmeow is this checkout; only megpt-mono can be missing.
clone_repo_if_missing megpt-mono

MEGPT="$(authintel_repo_path megpt-mono)"
AURARN="$(authintel_repo_path auraRN)"
HONO="$(authintel_repo_path aura-hono-api)"

if [[ ! -d "$AURARN" || ! -d "$HONO" ]]; then
  echo "[cloud-install] megpt-mono at $MEGPT is missing auraRN/ or aura-hono-api/." >&2
  exit 1
fi

AURARN_BUN="$(authintel_read_bun_pin "$AURARN")"
HONO_BUN="$(authintel_read_bun_pin "$HONO")"
if [[ -z "$AURARN_BUN" || -z "$HONO_BUN" ]]; then
  echo "[cloud-install] Could not read packageManager bun pins from megpt-mono trees." >&2
  exit 1
fi
if [[ "$AURARN_BUN" != "$HONO_BUN" ]]; then
  echo "[cloud-install] Bun pins diverge inside megpt-mono: auraRN=$AURARN_BUN aura-hono-api=$HONO_BUN. The monorepo runs one bun." >&2
  exit 1
fi
MEGPT_BUN="$HONO_BUN"
echo "[cloud-install] Bun pin: $MEGPT_BUN (both megpt-mono trees)."

# --- megpt-mono (shared Bun; backend tree owns ~/.bun) -----------------------
echo "[cloud-install] Installing megpt-mono at $MEGPT."
if [[ -n "${AURADB_EDGEDB_DSN:-}" && -n "${INSTANT_APP_ADMIN_TOKEN:-}" ]]; then
  echo "[cloud-install] Sibling API secrets present in this process; running megpt-mono install.sh."
  bash "$MEGPT/scripts/cloud/install.sh"
else
  echo "[cloud-install] Sibling API secrets not injected into this Build (AURADB_EDGEDB_DSN and/or INSTANT_APP_ADMIN_TOKEN empty). User-scoped secrets are agent-runtime only. Skipping megpt-mono install.sh (it fail-closes) and installing Bun/Redis/lockfile deps plus auraRN from whatsmeow."
  bash "$WHATSMEOW_ROOT/scripts/cloud/install-sibling-api-deps.sh" "$MEGPT" "$MEGPT_BUN"
  echo "[cloud-install] Installing megpt-mono/auraRN (no secrets)."
  env -u AURADB_EDGEDB_DSN -u INSTANT_APP_ADMIN_TOKEN \
    -u ANTHROPIC_API_KEY -u GEMINI_API_KEY -u CURSOR_API_KEY -u GREPTILE_API_KEY \
    bash "$AURARN/scripts/cloud/install.sh"
fi

# --- this repo (Go) ----------------------------------------------------------
echo "[cloud-install] Installing whatsmeow (Go)."
bash "$WHATSMEOW_ROOT/scripts/cloud/install-go.sh"

# --- Re-assert the shared bun on Cloud's reset PATH --------------------------
if [[ -x "${HOME}/.bun/bin/bun" ]]; then
  authintel_persist_onto_cloud_path "${HOME}/.bun/bin/bun" "bun"
fi
if [[ -e "${HOME}/.bun/bin/bunx" ]]; then
  authintel_persist_onto_cloud_path "${HOME}/.bun/bin/bunx" "bunx"
fi

bash "$WHATSMEOW_ROOT/scripts/cloud/verify-env.sh"

echo "[cloud-install] Workspace done."
