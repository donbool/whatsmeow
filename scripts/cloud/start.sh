#!/usr/bin/env bash
# Cursor Cloud agent start (referenced by .cursor/environment.json).
#
# Builds preserve disk state only. When aura-hono-api is a sibling, start
# Redis. A whatsmeow-only agent has nothing to start.
set -euo pipefail

cd "$(dirname "$0")/../.."
# shellcheck source=workspace.sh
. "$(dirname "$0")/workspace.sh"

HONO="$(authintel_repo_path aura-hono-api)"
if [[ -x "$HONO/scripts/cloud/start.sh" ]]; then
  exec bash "$HONO/scripts/cloud/start.sh"
fi
echo "[cloud-start] aura-hono-api not present; nothing to start."
exit 0
