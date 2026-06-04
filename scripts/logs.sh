#!/usr/bin/env bash
# scripts/logs.sh — tail the compose service logs.
set -euo pipefail

cd "$(dirname "$0")/.."
exec docker compose logs -f --tail=100 "$@"
