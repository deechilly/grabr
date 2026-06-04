#!/usr/bin/env bash
# scripts/up.sh — build the image and bring the service up in the background.
#
# Requires .env with GRABR_ADMIN_USER and GRABR_ADMIN_PASS (compose refuses to
# start without them). Run scripts/logs.sh after to tail output.
set -euo pipefail

cd "$(dirname "$0")/.."

if [[ ! -f .env ]]; then
  echo "missing .env — copy .env.example to .env and set credentials" >&2
  exit 1
fi

exec docker compose up -d --build "$@"
