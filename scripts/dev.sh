#!/usr/bin/env bash
# scripts/dev.sh — run grabr locally via `go run`.
#
# Loads credentials from .env if present, otherwise uses sensible defaults
# (admin/test). Persists state under ./data so it survives restarts.
set -euo pipefail

cd "$(dirname "$0")/.."

if [[ -f .env ]]; then
  # shellcheck disable=SC1091
  set -a; source .env; set +a
fi

: "${GRABR_ADMIN_USER:=admin}"
: "${GRABR_ADMIN_PASS:=test}"
: "${GRABR_PORT:=8080}"
: "${GRABR_DATA_DIR:=./data}"

export GRABR_ADMIN_USER GRABR_ADMIN_PASS GRABR_PORT GRABR_DATA_DIR

echo "grabr (dev) | port=$GRABR_PORT data=$GRABR_DATA_DIR user=$GRABR_ADMIN_USER"
exec go run ./cmd/grabr
