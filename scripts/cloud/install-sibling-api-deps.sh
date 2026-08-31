#!/usr/bin/env bash
# Secretless megpt-mono backend bootstrap for Cursor Cloud Builds.
#
# megpt-mono's install.sh fail-closes when AURADB_EDGEDB_DSN or
# INSTANT_APP_ADMIN_TOKEN is unset (it runs aura-hono-api/scripts/cloud/install.sh
# first). Cursor Builds do not inject user-scoped secrets
# (https://cursor.com/docs/cloud-agent/builds), so calling that script from
# install-workspace.sh kills every recurring Build.
#
# This script lives in whatsmeow and does not modify megpt-mono. It installs
# the monorepo's pinned Bun, Redis, and aura-hono-api lockfile deps only — no
# Gel codegen, no verify:env, no local Gel instance. auraRN is installed by
# the caller (its own install.sh needs no secrets).
set -euo pipefail

cd "$(dirname "$0")/../.."
# shellcheck source=workspace.sh
. "$(dirname "$0")/workspace.sh"

MEGPT="${1:?megpt-mono checkout path}"
MEGPT_BUN="${2:?megpt-mono bun pin}"
HONO="$MEGPT/aura-hono-api"

if [[ ! -f "$HONO/package.json" ]]; then
  echo "[cloud-install] megpt-mono checkout missing aura-hono-api/package.json at $HONO." >&2
  exit 1
fi

echo "[cloud-install] Secretless megpt-mono API deps at $HONO (bun $MEGPT_BUN). Skipping megpt-mono install.sh."

export BUN_INSTALL="${HOME}/.bun"
export PATH="${BUN_INSTALL}/bin:${PATH}"
if ! command -v bun >/dev/null 2>&1 || [[ "$(bun --version)" != "$MEGPT_BUN" ]]; then
  echo "[cloud-install] Installing Bun v$MEGPT_BUN for the megpt-mono pin."
  curl -fsSL https://bun.sh/install | bash -s "bun-v${MEGPT_BUN}"
fi
if [[ ! -x "${BUN_INSTALL}/bin/bun" ]]; then
  echo "[cloud-install] bun missing at ${BUN_INSTALL}/bin/bun after install." >&2
  exit 1
fi
# Bun 1.4.0's installer writes `bun` only; lockfile scripts and later
# agent-start codegen invoke bunx.
if [[ ! -e "${BUN_INSTALL}/bin/bunx" ]]; then
  ln -sfn "${BUN_INSTALL}/bin/bun" "${BUN_INSTALL}/bin/bunx"
fi
authintel_persist_onto_cloud_path "${BUN_INSTALL}/bin/bun" "bun"
authintel_persist_onto_cloud_path "${BUN_INSTALL}/bin/bunx" "bunx"

if ! command -v redis-server >/dev/null 2>&1; then
  echo "[cloud-install] Installing redis-server."
  sudo DEBIAN_FRONTEND=noninteractive apt-get update -qq
  sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -qq redis-server
fi

echo "[cloud-install] bun install --frozen-lockfile in megpt-mono/aura-hono-api."
(
  cd "$HONO"
  bun install --frozen-lockfile
)
for sub in tool referral pi-agent; do
  if [[ -f "$HONO/$sub/package.json" ]]; then
    echo "[cloud-install] bun install --frozen-lockfile in megpt-mono/aura-hono-api/$sub/."
    (
      cd "$HONO/$sub"
      bun install --frozen-lockfile
    )
  fi
done

if redis-cli ping >/dev/null 2>&1; then
  echo "[cloud-install] Redis already running on :6379."
else
  sudo -n service redis-server start >/dev/null 2>&1 || redis-server --daemonize yes >/dev/null 2>&1 || true
  for _ in $(seq 1 20); do
    if redis-cli ping >/dev/null 2>&1; then
      echo "[cloud-install] Redis is up on :6379."
      break
    fi
    sleep 0.5
  done
fi
if ! redis-cli ping >/dev/null 2>&1; then
  echo "[cloud-install] Redis failed to start on :6379." >&2
  exit 1
fi

echo "[cloud-install] Secretless megpt-mono API deps done."
