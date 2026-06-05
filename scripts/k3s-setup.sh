#!/usr/bin/env bash
# k3s-setup.sh — deploy grabr to a remote Arch Linux box via SSH using k3s.
#
# The image is built on the NAS (avoids local Docker bridge issues), then
# piped directly into k3s containerd. No local Docker required.
#
# Usage:
#   ./scripts/k3s-setup.sh              # deploy / update image + manifests
#   ./scripts/k3s-setup.sh --migrate    # also migrate data from Docker volume
#   ./scripts/k3s-setup.sh --reinstall  # wipe k3s and start fresh (DESTRUCTIVE)
#
# Prerequisites (local machine):
#   - kubectl, ssh, tar, go, docker
#   - SSH key access: ssh root@192.168.1.7
#
# Prerequisites (NAS):
#   - docker (pacman -S docker && systemctl enable --now docker)
#   - go     (pacman -S go)   — only needed for --migrate
#
# The image is built locally using Dockerfile.prebuilt (no RUN commands, so no
# Docker container is created and no bridge/veth setup is required). The binary
# is cross-compiled with go build, wrapped in distroless, then piped into the
# NAS k3s containerd.
#
# Credentials: fill in k8s/01-secrets.yaml from k8s/01-secrets.yaml.example

set -euo pipefail

# ---- config ----------------------------------------------------------------
NAS_HOST="${NAS_HOST:-root@192.168.1.7}"
NAMESPACE="${GRABR_NAMESPACE:-grabr}"
IMAGE="${GRABR_IMAGE:-grabr:dev}"
KUBECONFIG_PATH="${KUBECONFIG_PATH:-$HOME/.kube/config-grabr}"
NAS_BUILD_DIR="${NAS_BUILD_DIR:-/tmp/grabr-build}"
DOCKER_VOLUME_DATA="${DOCKER_VOLUME_DATA:-/var/lib/docker/volumes/grabr_grabr-data/_data}"
# ----------------------------------------------------------------------------

MIGRATE=false
REINSTALL=false
for arg in "$@"; do
    case $arg in
        --migrate)   MIGRATE=true ;;
        --reinstall) REINSTALL=true ;;
        *) echo "Unknown arg: $arg" >&2; exit 1 ;;
    esac
done

log()  { echo "▶ $*"; }
ok()   { echo "✓ $*"; }
die()  { echo "✗ ERROR: $*" >&2; exit 1; }
nas()  { ssh "$NAS_HOST" "$@"; }

# ---- 0. Prereqs ------------------------------------------------------------
command -v kubectl >/dev/null 2>&1 || die "kubectl not found locally"
command -v go     >/dev/null 2>&1  || die "go not found locally (needed to cross-compile the binary)"
command -v docker >/dev/null 2>&1  || die "docker not found locally (needed to wrap binary in distroless)"
ssh -o BatchMode=yes -o ConnectTimeout=5 "$NAS_HOST" true 2>/dev/null \
    || die "Cannot SSH to $NAS_HOST — check your SSH key"

# ---- 1. Install / verify k3s -----------------------------------------------
if $REINSTALL; then
    log "Uninstalling k3s (--reinstall)..."
    nas 'k3s-uninstall.sh 2>/dev/null || true'
fi

if ! nas 'command -v k3s >/dev/null 2>&1'; then
    log "Installing k3s on $NAS_HOST..."
    nas 'curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="server --disable=traefik" sh -'
    log "Waiting for k3s node to be Ready..."
    nas 'until k3s kubectl get nodes 2>/dev/null | grep -q " Ready"; do sleep 2; done'
    ok "k3s installed"
else
    ok "k3s already installed"
fi

# ---- 2. Fetch kubeconfig ---------------------------------------------------
log "Fetching kubeconfig from $NAS_HOST..."
mkdir -p "$(dirname "$KUBECONFIG_PATH")"
NAS_IP=$(echo "$NAS_HOST" | sed 's/.*@//')
nas 'cat /etc/rancher/k3s/k3s.yaml' \
    | sed "s/127.0.0.1/$NAS_IP/g" \
    > "$KUBECONFIG_PATH"
chmod 600 "$KUBECONFIG_PATH"
export KUBECONFIG="$KUBECONFIG_PATH"
ok "kubeconfig → $KUBECONFIG_PATH"
log "Add to shell profile: export KUBECONFIG=$KUBECONFIG_PATH"

# ---- 3. Namespace + secrets + manifests ------------------------------------
kubectl apply -f k8s/00-namespace.yaml

[[ -f k8s/01-secrets.yaml ]] \
    || die "k8s/01-secrets.yaml not found — copy k8s/01-secrets.yaml.example, fill in values, re-run"
kubectl apply -f k8s/01-secrets.yaml
ok "secrets applied"

log "Applying storage, Postgres, and portal manifests..."
kubectl apply -f k8s/02-storage.yaml
kubectl apply -f k8s/03-postgres.yaml
kubectl apply -f k8s/04-portal.yaml

log "Waiting for Postgres to be ready..."
# kubectl wait fails with "no matching resources found" if the pod isn't
# scheduled yet (common after a fresh k3s install while the image pulls).
# Poll until the pod exists, then wait for Ready.
until kubectl get pod -n "$NAMESPACE" -l app=postgres 2>/dev/null | grep -q postgres; do
    echo "  waiting for postgres pod to be scheduled..."
    sleep 5
done
kubectl wait pod -n "$NAMESPACE" -l app=postgres \
    --for=condition=Ready --timeout=180s
ok "Postgres ready"

# ---- 4. Build binary locally and wrap in distroless ------------------------
# Cross-compile for the NAS (linux/amd64). Dockerfile.prebuilt has no RUN
# instructions so Docker never creates a container — no bridge/veth needed.
log "Cross-compiling grabr for linux/amd64..."
mkdir -p bin
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o bin/grabr-linux ./cmd/grabr
ok "Binary compiled → bin/grabr-linux"

log "Building image $IMAGE (Dockerfile.prebuilt, no RUN steps)..."
docker build -f Dockerfile.prebuilt -t "$IMAGE" .
ok "Image built"

log "Importing image into k3s containerd on $NAS_HOST..."
docker save "$IMAGE" | ssh "$NAS_HOST" "k3s ctr images import -"
ok "Image $IMAGE imported"

# ---- 5. Roll out portal ----------------------------------------------------
log "Rolling out portal..."
kubectl rollout restart deployment/grabr-portal -n "$NAMESPACE" 2>/dev/null || true
kubectl rollout status deployment/grabr-portal -n "$NAMESPACE" --timeout=120s \
    || {
        echo ""
        echo "  Portal rollout timed out. Pod state:"
        kubectl get pod -n "$NAMESPACE" -l app=grabr-portal
        echo ""
        echo "  Logs (last 40 lines):"
        kubectl logs -n "$NAMESPACE" -l app=grabr-portal --tail=40 2>/dev/null || true
        die "Portal did not become ready — see logs above"
    }
ok "Portal deployed"

# ---- 6. Optional: migrate from Docker volume -------------------------------
if $MIGRATE; then
    log "Running migration from Docker volume on $NAS_HOST..."

    SQLITE_PATH="$DOCKER_VOLUME_DATA/grabr.db"
    nas "test -f '$SQLITE_PATH'" \
        || die "grabr.db not found at $SQLITE_PATH — set DOCKER_VOLUME_DATA if the path differs"

    log "Copying grabr binary to $NAS_HOST..."
    scp bin/grabr-linux "${NAS_HOST}:/tmp/grabr-bin"
    ok "Binary copied"

    # Pull the DATABASE_URL from the secret we already applied.
    DB_URL=$(kubectl get secret grabr-db -n "$NAMESPACE" \
        -o jsonpath='{.data.DATABASE_URL}' | base64 -d)
    # Rewrite cluster-internal hostname → localhost for the port-forward.
    LOCAL_DSN=$(echo "$DB_URL" | sed 's|@postgres\.[^/]*/|@127.0.0.1:15432/|')

    log "Migrating database on $NAS_HOST..."
    # Run port-forward + migrate in a single SSH session so cleanup is automatic.
    nas "
set -e
k3s kubectl port-forward -n '$NAMESPACE' svc/postgres 15432:5432 &
PF_PID=\$!
sleep 2
/tmp/grabr-bin migrate --sqlite '$SQLITE_PATH' --postgres '$LOCAL_DSN'
kill \$PF_PID 2>/dev/null || true
"
    ok "Database migrated"

    # Copy mirrors + backups from the Docker volume into the PVC.
    # The portal pod already mounts the PVC at /data, so we pipe tar through
    # kubectl exec — the data goes NAS disk → kubectl exec → PVC, no local copy.
    PORTAL_POD=$(kubectl get pod -n "$NAMESPACE" -l app=grabr-portal \
        -o jsonpath='{.items[0].metadata.name}')

    for dir in mirrors backups; do
        if nas "test -d '$DOCKER_VOLUME_DATA/$dir'"; then
            log "Copying $dir/ into PVC via portal pod..."
            nas "tar -C '$DOCKER_VOLUME_DATA' -cf - '$dir'" \
                | kubectl exec -i -n "$NAMESPACE" "$PORTAL_POD" -- tar -C /data -xf -
            ok "$dir/ copied"
        else
            log "$dir/ not found on NAS; skipping"
        fi
    done
fi

# ---- 7. Done ---------------------------------------------------------------
NODE_IP=$(nas 'hostname -I | awk "{print \$1}"')
echo ""
ok "Deployment complete!"
echo ""
echo "  Access grabr at:  http://${NODE_IP}:30080"
echo "  Kubeconfig:       $KUBECONFIG_PATH"
echo ""
echo "  Useful commands:"
echo "    kubectl -n $NAMESPACE get pods"
echo "    kubectl -n $NAMESPACE logs deploy/grabr-portal -f"
echo ""
echo "  To redeploy after code changes:  ./scripts/k3s-setup.sh"
echo "  To migrate Docker data:          ./scripts/k3s-setup.sh --migrate"
