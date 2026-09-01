#!/usr/bin/env bash
# Build hairpin, load it into the kind cluster, and apply the reference
# manifests plus the development-only fake provider.
#
# The reference manifests name a published image; this script overrides
# that with the locally built one, which never leaves the cluster's
# image store (hence imagePullPolicy: IfNotPresent).
#
# Billet gets the same treatment when a sibling checkout is present at
# BILLET_DIR, so a change to the memory store can be exercised without
# publishing it.

set -euo pipefail

CLUSTER_NAME="${HAIRPIN_CLUSTER_NAME:-hairpin}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
IMAGE="localhost/hairpin:dev"
BILLET_DIR="${BILLET_DIR:-${REPO_ROOT}/../billet}"
BILLET_IMAGE="localhost/billet:dev"

export KIND_EXPERIMENTAL_PROVIDER="${KIND_EXPERIMENTAL_PROVIDER:-podman}"
ENGINE="${HAIRPIN_CONTAINER_ENGINE:-podman}"

log()  { printf '[deploy] %s\n' "$*"; }
fail() { printf '[deploy] ERROR: %s\n' "$*" >&2; exit 1; }

# Point a Deployment at an image held only in the cluster's own store.
# The strategic-merge patch is keyed on the container name, so it adds
# or replaces imagePullPolicy without needing to know which.
pin_local_image() {
    local deployment="$1" container="$2" image="$3"
    kubectl -n hairpin set image "deployment/${deployment}" "${container}=${image}"
    kubectl -n hairpin patch deployment "${deployment}" -p \
        "{\"spec\":{\"template\":{\"spec\":{\"containers\":[{\"name\":\"${container}\",\"imagePullPolicy\":\"IfNotPresent\"}]}}}}"
}

# `kind get clusters` is unusable against this podman (see kind-up.sh);
# kind's node container is the reliable signal.
"${ENGINE}" container exists "${CLUSTER_NAME}-control-plane" 2>/dev/null \
    || fail "cluster '${CLUSTER_NAME}' does not exist; run scripts/dev/kind-up.sh"

archive="$(mktemp -t hairpin-image-XXXXXX).tar"
billet_archive=""
billet_local=false
trap 'rm -f "${archive}" "${billet_archive}"' EXIT

log "building ${IMAGE}..."
"${ENGINE}" build -t "${IMAGE}" -f "${REPO_ROOT}/Containerfile" "${REPO_ROOT}"

log "loading ${IMAGE} into the cluster..."
"${ENGINE}" save --format oci-archive -o "${archive}" "${IMAGE}"
kind load image-archive "${archive}" --name "${CLUSTER_NAME}"

if [ -f "${BILLET_DIR}/Containerfile" ]; then
    billet_archive="$(mktemp -t billet-image-XXXXXX).tar"

    log "building ${BILLET_IMAGE} from ${BILLET_DIR}..."
    (cd "${BILLET_DIR}" && "${ENGINE}" build -t "${BILLET_IMAGE}" -f Containerfile .)

    log "loading ${BILLET_IMAGE} into the cluster..."
    "${ENGINE}" save --format oci-archive -o "${billet_archive}" "${BILLET_IMAGE}"
    kind load image-archive "${billet_archive}" --name "${CLUSTER_NAME}"
    billet_local=true
else
    log "no Billet checkout at ${BILLET_DIR}; the published image will be pulled"
fi

log "applying manifests..."
kubectl apply \
    -f "${REPO_ROOT}/examples/k8s/namespace.yaml" \
    -f "${REPO_ROOT}/examples/k8s/rbac.yaml" \
    -f "${REPO_ROOT}/examples/k8s/rbac-sandbox.yaml" \
    -f "${REPO_ROOT}/examples/k8s/redis.yaml" \
    -f "${REPO_ROOT}/examples/k8s/billet.yaml" \
    -f "${REPO_ROOT}/examples/k8s/profiles.yaml" \
    -f "${REPO_ROOT}/examples/k8s/hairpin.yaml"

# The fake provider ships its own hairpin-profiles ConfigMap, replacing
# the reference one so the default profile points at it.
log "applying the development fake provider..."
kubectl -n hairpin create secret generic provider-api-keys \
    --from-literal=FAKE_API_KEY=not-a-real-key \
    --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f "${SCRIPT_DIR}/fake-provider.yaml"

log "pointing the Deployments at the locally built images..."
pin_local_image hairpin hairpin "${IMAGE}"
if [ "${billet_local}" = true ]; then
    pin_local_image billet billet "${BILLET_IMAGE}"
fi

log "waiting for rollouts..."
kubectl -n hairpin rollout status deployment/redis --timeout=120s
kubectl -n hairpin rollout status deployment/billet --timeout=120s
# The provider reads server.py once at start, so a changed ConfigMap
# only takes effect on a fresh Pod.
kubectl -n hairpin rollout restart deployment/fake-provider
kubectl -n hairpin rollout status deployment/fake-provider --timeout=120s
kubectl -n hairpin rollout restart deployment/hairpin
kubectl -n hairpin rollout status deployment/hairpin --timeout=120s

log "done. Next: scripts/dev/smoke-test.sh"
