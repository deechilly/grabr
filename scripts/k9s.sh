#!/usr/bin/env bash
# k9s.sh — launch k9s locally against the remote k3s cluster.
#
# Uses the kubeconfig that scripts/k3s-setup.sh wrote out (default:
# ~/.kube/config-grabr). The kubeconfig already points at the NAS, so this
# is just a thin wrapper that sets KUBECONFIG and lands you in the grabr
# namespace.

set -euo pipefail

KUBECONFIG_PATH="${KUBECONFIG_PATH:-$HOME/.kube/config-grabr}"
NAMESPACE="${GRABR_NAMESPACE:-grabr}"

if ! command -v k9s >/dev/null 2>&1; then
    echo "k9s not found. Install with:  brew install k9s" >&2
    exit 1
fi

if [[ ! -f "$KUBECONFIG_PATH" ]]; then
    echo "kubeconfig not found at $KUBECONFIG_PATH" >&2
    echo "Run ./scripts/k3s-setup.sh first to deploy and fetch the kubeconfig." >&2
    exit 1
fi

export KUBECONFIG="$KUBECONFIG_PATH"
exec k9s -n "$NAMESPACE" "$@"
