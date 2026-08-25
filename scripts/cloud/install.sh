#!/usr/bin/env bash
# Cursor Cloud agent install (referenced by .cursor/environment.json).
#
# This repo is the environment.json home for the authintel multi-repo
# environment. Cursor runs this from the whatsmeow root:
#   - Multi-repo (auraRN + aura-hono-api cloned as siblings): full workspace.
#   - This repo alone: Go toolchain + tests only (install-go.sh).
set -euo pipefail

cd "$(dirname "$0")/../.."
# shellcheck source=workspace.sh
. "$(dirname "$0")/workspace.sh"

if authintel_has_workspace_siblings; then
  exec bash "$(dirname "$0")/install-workspace.sh"
fi
exec bash "$(dirname "$0")/install-go.sh"
