#!/usr/bin/env bash
# migrate.sh — migrate an existing Docker-based grabr installation to Kubernetes.
#
# What this script does:
#   1. Locates the grabr Docker volume and copies grabr.db to a temp dir.
#   2. Port-forwards the Postgres pod so the migration tool can reach it.
#   3. Runs `grabr migrate` to copy the database from SQLite → Postgres.
#   4. Rsyncs mirror and backup files from the Docker volume into the k8s PVC
#      via a temporary rsync pod.
#
# Prerequisites:
#   - grabr binary is built and on PATH (go build -o ./bin/grabr ./cmd/grabr)
#   - kubectl is configured and pointed at your kind cluster
#   - The Kubernetes resources in k8s/ are already applied
#     (kubectl apply -f k8s/)
#   - Postgres pod is ready
#   - Docker is running and the grabr container has the named volume

set -euo pipefail

# ---- config ----------------------------------------------------------------
VOLUME_DATA="${GRABR_VOLUME_DATA:-/var/lib/docker/volumes/grabr_grabr-data/_data}"
NAMESPACE="${GRABR_NAMESPACE:-grabr}"
POSTGRES_SECRET="${GRABR_DB_SECRET:-grabr-db}"
GRABR_BIN="${GRABR_BIN:-./bin/grabr}"
INCLUDE_VISITED="${INCLUDE_VISITED:-false}"
# ----------------------------------------------------------------------------

log() { echo "[migrate] $*"; }
die() { echo "[migrate] ERROR: $*" >&2; exit 1; }

# Validate inputs
[[ -d "$VOLUME_DATA" ]] || die "Docker volume not found at $VOLUME_DATA (set GRABR_VOLUME_DATA)"
[[ -f "$GRABR_BIN" ]] || die "grabr binary not found at $GRABR_BIN (run: go build -o ./bin/grabr ./cmd/grabr)"
command -v kubectl >/dev/null 2>&1 || die "kubectl not found in PATH"

SQLITE_PATH="$VOLUME_DATA/grabr.db"
[[ -f "$SQLITE_PATH" ]] || die "grabr.db not found at $SQLITE_PATH"

# Fetch Postgres DSN from the k8s secret
log "Reading DATABASE_URL from secret $POSTGRES_SECRET in namespace $NAMESPACE..."
DSN=$(kubectl get secret "$POSTGRES_SECRET" -n "$NAMESPACE" \
    -o jsonpath='{.data.DATABASE_URL}' | base64 -d)
[[ -n "$DSN" ]] || die "DATABASE_URL not found in secret $POSTGRES_SECRET"

# Wait for Postgres to be ready
log "Waiting for Postgres pod to be ready..."
kubectl wait pod -n "$NAMESPACE" -l app=postgres \
    --for=condition=Ready --timeout=120s

# Port-forward Postgres in the background
log "Port-forwarding postgres:5432 → localhost:15432..."
kubectl port-forward -n "$NAMESPACE" svc/postgres 15432:5432 &
PF_PID=$!
trap 'kill $PF_PID 2>/dev/null || true' EXIT
sleep 2  # give port-forward time to bind

# Rewrite DSN to point at the local port-forward
LOCAL_DSN=$(echo "$DSN" | sed 's|@[^/]*\.cluster\.local:\([0-9]*\)/|@localhost:15432/|g')
# If the above sed didn't match (custom DSN format), try a simpler replacement
if echo "$LOCAL_DSN" | grep -q "cluster.local"; then
    # fallback: extract db name and build a fresh DSN
    PGPASS=$(kubectl get secret "$POSTGRES_SECRET" -n "$NAMESPACE" \
        -o jsonpath='{.data.POSTGRES_PASSWORD}' | base64 -d)
    LOCAL_DSN="postgres://grabr:${PGPASS}@localhost:15432/grabr?sslmode=disable"
fi

log "Running database migration..."
MIGRATE_ARGS=(migrate --sqlite "$SQLITE_PATH" --postgres "$LOCAL_DSN")
if [[ "$INCLUDE_VISITED" == "true" ]]; then
    MIGRATE_ARGS+=(--include-visited)
fi
"$GRABR_BIN" "${MIGRATE_ARGS[@]}"

# Stop port-forward
kill "$PF_PID" 2>/dev/null || true
trap - EXIT

# ---- File migration --------------------------------------------------------
log "Migrating mirror and backup files into the PVC..."

MIRRORS_SRC="$VOLUME_DATA/mirrors"
BACKUPS_SRC="$VOLUME_DATA/backups"

# Find the portal pod to use as an rsync target
PORTAL_POD=$(kubectl get pod -n "$NAMESPACE" -l app=grabr-portal \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)

if [[ -z "$PORTAL_POD" ]]; then
    log "Portal pod not running. Launching a temporary rsync pod instead..."
    kubectl run grabr-migrate-rsync \
        --image=alpine \
        --restart=Never \
        --rm \
        -n "$NAMESPACE" \
        --overrides='{
          "spec": {
            "containers": [{
              "name": "grabr-migrate-rsync",
              "image": "alpine",
              "command": ["sleep", "3600"],
              "volumeMounts": [{"name":"mirrors","mountPath":"/data"}]
            }],
            "volumes": [{"name":"mirrors","persistentVolumeClaim":{"claimName":"grabr-mirrors"}}]
          }
        }' &
    sleep 5
    PORTAL_POD=$(kubectl get pod -n "$NAMESPACE" grabr-migrate-rsync \
        -o jsonpath='{.metadata.name}' 2>/dev/null || true)
    CLEANUP_POD=true
else
    CLEANUP_POD=false
fi

if [[ -n "$PORTAL_POD" ]]; then
    if [[ -d "$MIRRORS_SRC" ]]; then
        log "Copying mirrors/ → pod:$PORTAL_POD:/data/mirrors/ ..."
        kubectl cp "$MIRRORS_SRC/." "$NAMESPACE/$PORTAL_POD:/data/mirrors/"
    else
        log "No mirrors/ directory found; skipping."
    fi

    if [[ -d "$BACKUPS_SRC" ]]; then
        log "Copying backups/ → pod:$PORTAL_POD:/data/backups/ ..."
        kubectl cp "$BACKUPS_SRC/." "$NAMESPACE/$PORTAL_POD:/data/backups/"
    else
        log "No backups/ directory found; skipping."
    fi

    if [[ "$CLEANUP_POD" == "true" ]]; then
        kubectl delete pod -n "$NAMESPACE" grabr-migrate-rsync --ignore-not-found
    fi
else
    log "WARNING: Could not find a pod to copy files into."
    log "Copy manually:"
    log "  kubectl cp $MIRRORS_SRC/. $NAMESPACE/<pod>:/data/mirrors/"
    log "  kubectl cp $BACKUPS_SRC/. $NAMESPACE/<pod>:/data/backups/"
fi

log ""
log "Migration complete!"
log "Next steps:"
log "  1. Apply the portal deployment if not yet done:"
log "     kubectl apply -f k8s/04-portal.yaml"
log "  2. Load the grabr image into kind:"
log "     kind load docker-image grabr:dev"
log "  3. Watch the portal come up:"
log "     kubectl rollout status deployment/grabr-portal -n $NAMESPACE"
log "  4. Access via http://localhost:30080 (or the NodePort you configured)"
