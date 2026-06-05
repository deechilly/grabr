#!/usr/bin/env bash
# k3s-setup.sh — deploy grabr to a remote linux box via SSH using k3s.
#
# The grabr binary is cross-compiled to linux/amd64 on the local machine and
# wrapped in a distroless image using Dockerfile.prebuilt. The Dockerfile has
# no RUN steps so Docker never spins up a container — useful on hosts where
# the Docker default bridge is broken. The resulting image is streamed over
# SSH into k3s' containerd image store on the target node.
#
# Usage:
#   ./scripts/k3s-setup.sh              # deploy / update image + manifests
#   ./scripts/k3s-setup.sh --migrate    # also migrate data from Docker volume
#   ./scripts/k3s-setup.sh --reinstall  # wipe k3s and start fresh (DESTRUCTIVE)
#
# Prerequisites (local machine):
#   - kubectl, ssh, tar, go, docker (Docker Desktop / OrbStack / colima all work)
#   - SSH key access to $NAS_HOST (e.g. root@your-nas-ip; set in .env)
#
# Prerequisites (NAS):
#   - k3s installed by this script if absent
#   - No other tooling required; --migrate copies the grabr binary over via scp
#     and runs it against a port-forwarded postgres
#
# Credentials: fill in k8s/01-secrets.yaml from k8s/01-secrets.yaml.example

set -euo pipefail

# Source project-root .env if present, so NAS_HOST and other deploy vars stay
# out of version control. .env is in .gitignore; .env.example is the template.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
if [[ -f "$REPO_ROOT/.env" ]]; then
    set -a
    # shellcheck disable=SC1091
    . "$REPO_ROOT/.env"
    set +a
fi

# ---- config ----------------------------------------------------------------
NAS_HOST="${NAS_HOST:-}"
NAMESPACE="${GRABR_NAMESPACE:-grabr}"
IMAGE="${GRABR_IMAGE:-grabr:dev}"
KUBECONFIG_PATH="${KUBECONFIG_PATH:-$HOME/.kube/config-grabr}"
NAS_BUILD_DIR="${NAS_BUILD_DIR:-/tmp/grabr-build}"
DOCKER_VOLUME_DATA="${DOCKER_VOLUME_DATA:-/var/lib/docker/volumes/grabr_grabr-data/_data}"
# ----------------------------------------------------------------------------

[[ -n "$NAS_HOST" ]] || {
    echo "✗ ERROR: NAS_HOST is not set." >&2
    echo "  Add it to $REPO_ROOT/.env (see .env.example) or export it in your shell," >&2
    echo "  e.g.  NAS_HOST=root@your-nas-ip ./scripts/k3s-setup.sh" >&2
    exit 1
}

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
# --platform linux/amd64 is required on Apple Silicon: docker defaults to the
# host arch, which would produce an arm64 image that an amd64 k3s node refuses
# to schedule with ErrImageNeverPull. Harmless on amd64 hosts.
docker build --platform linux/amd64 -f Dockerfile.prebuilt -t "$IMAGE" .
ok "Image built"

log "Importing image into k3s containerd on $NAS_HOST..."
# Pipe directly into k3s' bundled containerd (namespace k8s.io, the default
# for `k3s ctr`). Then tag the unqualified alias so the manifest's
# `image: grabr:dev` matches a registered name — without the alias the kubelet
# fails with ErrImageNeverPull when imagePullPolicy=Never.
docker save "$IMAGE" | ssh "$NAS_HOST" "k3s ctr images import -"
ssh "$NAS_HOST" "k3s ctr images tag --force docker.io/library/$IMAGE $IMAGE >/dev/null"
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
    # The portal image is distroless (no shell, no tar), so we can't exec into
    # it. Spin up a short-lived alpine pod that mounts the same PVC and stream
    # tar through it: NAS disk → kubectl exec → tar -x → PVC. No local copy.
    RSYNC_POD=grabr-migrate-rsync
    log "Launching temporary $RSYNC_POD pod to receive files..."
    kubectl delete pod -n "$NAMESPACE" "$RSYNC_POD" --ignore-not-found --wait=true >/dev/null
    kubectl apply -n "$NAMESPACE" -f - <<EOF >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: $RSYNC_POD
  labels:
    app.kubernetes.io/managed-by: grabr-migrate
spec:
  restartPolicy: Never
  containers:
    - name: rsync
      image: alpine:3.19
      command: ["sleep", "3600"]
      volumeMounts:
        - { name: mirrors, mountPath: /data }
  volumes:
    - name: mirrors
      persistentVolumeClaim:
        claimName: grabr-mirrors
EOF
    kubectl wait pod -n "$NAMESPACE" "$RSYNC_POD" --for=condition=Ready --timeout=120s >/dev/null
    ok "$RSYNC_POD ready"

    for dir in mirrors backups; do
        if nas "test -d '$DOCKER_VOLUME_DATA/$dir'"; then
            log "Copying $dir/ into PVC via $RSYNC_POD..."
            nas "tar -C '$DOCKER_VOLUME_DATA' -cf - '$dir'" \
                | kubectl exec -i -n "$NAMESPACE" "$RSYNC_POD" -- tar -C /data -xf -
            ok "$dir/ copied"
        else
            log "$dir/ not found on NAS; skipping"
        fi
    done

    log "Removing $RSYNC_POD..."
    kubectl delete pod -n "$NAMESPACE" "$RSYNC_POD" --ignore-not-found --wait=false >/dev/null
fi

# ---- 7. Done ---------------------------------------------------------------
# Derive node IP from $NAS_HOST rather than asking the NAS — `hostname` is not
# always on PATH for non-login SSH sessions.
NODE_IP="${NAS_HOST#*@}"
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
