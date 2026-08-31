#!/usr/bin/env bash
# Cursor Cloud agent start (referenced by .cursor/environment.json).
#
# Builds preserve disk state only. When megpt-mono is a sibling, start its
# daemons (Redis). A whatsmeow-only agent has nothing to start.
set -euo pipefail

cd "$(dirname "$0")/../.."
# shellcheck source=workspace.sh
. "$(dirname "$0")/workspace.sh"

MEGPT="$(authintel_repo_path megpt-mono)"
if [[ -f "$MEGPT/scripts/cloud/start.sh" ]]; then
  exec bash "$MEGPT/scripts/cloud/start.sh"
fi
echo "[cloud-start] megpt-mono not present; nothing to start."
exit 0
