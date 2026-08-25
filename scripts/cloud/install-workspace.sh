#!/usr/bin/env bash
# Authintel multi-repo install. Called by install.sh when auraRN and/or
# aura-hono-api are siblings of this whatsmeow checkout.
#
# Idempotent. Delegates to each repo's own install:
#   auraRN          — Bun 1.3.5 + Swift + bun run verify:env
#   whatsmeow       — install-go.sh
#   aura-hono-api   — Bun 1.4.0 + Redis + Greptile + prod Gel/Instant
#
# The two Bun pins cannot share one PATH `bun`. auraRN's pin lives in
# $HOME/.bun-versions/<ver> as bun-<ver>; aura-hono-api's pin is default `bun`.
set -euo pipefail

cd "$(dirname "$0")/../.."
# shellcheck source=workspace.sh
. "$(dirname "$0")/workspace.sh"

echo "[cloud-install] Workspace: $AUTHINTEL_WORKSPACE (whatsmeow at $WHATSMEOW_ROOT)"

clone_repo_if_missing() {
  local name="$1"
  local dest
  dest="$(authintel_repo_path "$name")"
  if [[ -d "$dest/.git" || -f "$dest/package.json" || -f "$dest/go.mod" ]]; then
    echo "[cloud-install] $name already present at $dest."
    return 0
  fi
  local url
  url="$(authintel_repo_url "$name")"
  echo "[cloud-install] Cloning $name from $url."
  git clone "$url" "$dest"
}

# whatsmeow is this checkout; only the other two can be missing.
clone_repo_if_missing auraRN
clone_repo_if_missing aura-hono-api

AURARN="$(authintel_repo_path auraRN)"
HONO="$(authintel_repo_path aura-hono-api)"

AURARN_BUN="$(authintel_read_bun_pin "$AURARN")"
HONO_BUN="$(authintel_read_bun_pin "$HONO")"
if [[ -z "$AURARN_BUN" || -z "$HONO_BUN" ]]; then
  echo "[cloud-install] Could not read packageManager bun pins from auraRN and aura-hono-api." >&2
  exit 1
fi
echo "[cloud-install] Bun pins: auraRN=$AURARN_BUN aura-hono-api=$HONO_BUN"

# --- auraRN (isolated Bun so hono can own the default `bun` later) -----------
export BUN_INSTALL="${HOME}/.bun-versions/${AURARN_BUN}"
export PATH="${BUN_INSTALL}/bin:${PATH}"
mkdir -p "$BUN_INSTALL"
echo "[cloud-install] Installing auraRN with BUN_INSTALL=$BUN_INSTALL"
bash "$AURARN/scripts/cloud/install.sh"

# --- this repo (Go) ----------------------------------------------------------
echo "[cloud-install] Installing whatsmeow (Go)."
bash "$WHATSMEOW_ROOT/scripts/cloud/install-go.sh"

# --- aura-hono-api last so its bun + ~/.local/bin persist win the default ----
# Full codegen + verify:env need AURADB_EDGEDB_DSN and INSTANT_APP_ADMIN_TOKEN.
# Builds often do not inject those (user-scoped secrets are agent-runtime
# only). The sibling install then does toolchains + lockfile deps only;
# start.sh generates artifacts when an agent later has the secrets.
echo "[cloud-install] Installing sibling API repo at $HONO."
if [[ -n "${AURADB_EDGEDB_DSN:-}" && -n "${INSTANT_APP_ADMIN_TOKEN:-}" ]]; then
  echo "[cloud-install] Sibling API secrets present in this process (full install including codegen + verify:env)."
else
  echo "[cloud-install] Sibling API secrets not injected into this Build (AURADB_EDGEDB_DSN and/or INSTANT_APP_ADMIN_TOKEN empty). User-scoped secrets are agent-runtime only. Install will do deps only."
fi
bash "$HONO/scripts/cloud/install.sh"

# --- Re-assert versioned bun shims on Cloud's reset PATH ---------------------
AURARN_BUN_BIN="${HOME}/.bun-versions/${AURARN_BUN}/bin/bun"
AURARN_BUNX_BIN="${HOME}/.bun-versions/${AURARN_BUN}/bin/bunx"
if [[ ! -x "$AURARN_BUN_BIN" ]]; then
  echo "[cloud-install] auraRN bun missing at $AURARN_BUN_BIN after its install." >&2
  exit 1
fi
authintel_persist_onto_cloud_path "$AURARN_BUN_BIN" "bun-${AURARN_BUN}"
if [[ -e "$AURARN_BUNX_BIN" ]]; then
  authintel_persist_onto_cloud_path "$AURARN_BUNX_BIN" "bunx-${AURARN_BUN}"
fi
if [[ -x "${HOME}/.bun/bin/bun" ]]; then
  authintel_persist_onto_cloud_path "${HOME}/.bun/bin/bun" "bun-${HONO_BUN}"
fi

bash "$WHATSMEOW_ROOT/scripts/cloud/verify-env.sh"

echo "[cloud-install] Workspace done."
