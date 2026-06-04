#!/usr/bin/env bash
# scripts/reset.sh — DESTRUCTIVE: stop the service AND delete the named
# volume, wiping the SQLite DB plus every mirror and backup on it. Requires
# an explicit "yes" to proceed.
set -euo pipefail

cd "$(dirname "$0")/.."

cat <<'EOF'
This will run: docker compose down -v
That deletes the grabr-data named volume and everything in it:
  - the SQLite database (all configured sites + crawl history)
  - every mirrored site under /data/mirrors
  - every backup tar.gz under /data/backups

Type "yes" to continue, anything else to abort.
EOF

read -r answer
if [[ "$answer" != "yes" ]]; then
  echo "aborted."
  exit 1
fi

exec docker compose down -v "$@"
