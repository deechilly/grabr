#!/usr/bin/env bash
# scripts/down.sh — stop the service. The named volume (and your data) is
# preserved; use scripts/reset.sh if you actually want to wipe it.
set -euo pipefail

cd "$(dirname "$0")/.."
exec docker compose down "$@"
