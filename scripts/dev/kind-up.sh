#!/usr/bin/env bash
# Create the kind cluster hairpin's development loop runs on.
#
# kind drives podman here (KIND_EXPERIMENTAL_PROVIDER), which is what
# this machine has. Nothing is installed into the node beyond kind's own
# defaults: sandbox Pods run under the cluster-default RuntimeClass,
# which exercises every code path except the gVisor isolation boundary.

set -euo pipefail

CLUSTER_NAME="${HAIRPIN_CLUSTER_NAME:-hairpin}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

export KIND_EXPERIMENTAL_PROVIDER="${KIND_EXPERIMENTAL_PROVIDER:-podman}"

log()  { printf '[kind-up] %s\n' "$*"; }
fail() { printf '[kind-up] ERROR: %s\n' "$*" >&2; exit 1; }

# `kind get clusters` is unusable against this podman: it renders a Go
# template over `.Labels` that podman rejects. kind names its node
# container "<cluster>-control-plane", so ask the engine directly.
cluster_exists() {
    "${HAIRPIN_CONTAINER_ENGINE:-podman}" container exists \
        "${CLUSTER_NAME}-control-plane" 2>/dev/null
}

command -v kind >/dev/null || fail "kind is not in PATH"
command -v kubectl >/dev/null || fail "kubectl is not in PATH"

if cluster_exists; then
    log "cluster '${CLUSTER_NAME}' already exists; nothing to do"
    exit 0
fi

log "creating cluster '${CLUSTER_NAME}' (first run pulls the node image)..."
kind create cluster --name "${CLUSTER_NAME}" \
    --config "${SCRIPT_DIR}/kind-config.yaml" --wait 5m

log "done. Next: scripts/dev/deploy.sh"
