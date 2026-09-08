#!/usr/bin/env bash
# Build hairpin, load it into the kind cluster, and apply the reference
# manifests plus the development-only fake provider.
#
# The reference manifests name a published image; this script overrides
# that with the locally built one, which never leaves the cluster's
# image store (hence imagePullPolicy: IfNotPresent).
#
# Billet and steeplechase get the same treatment when a sibling checkout
# is present at BILLET_DIR or STEEPLECHASE_DIR, so a change to the memory
# store or the collector can be exercised without publishing it.

set -euo pipefail

CLUSTER_NAME="${HAIRPIN_CLUSTER_NAME:-hairpin}"
KUBE_CONTEXT="${HAIRPIN_KUBE_CONTEXT:-kind-${CLUSTER_NAME}}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
IMAGE="localhost/hairpin:dev"
BILLET_DIR="${BILLET_DIR:-${REPO_ROOT}/../billet}"
BILLET_IMAGE="localhost/billet:dev"
STEEPLECHASE_DIR="${STEEPLECHASE_DIR:-${REPO_ROOT}/../steeplechase}"
STEEPLECHASE_IMAGE="localhost/steeplechase:dev"

export KIND_EXPERIMENTAL_PROVIDER="${KIND_EXPERIMENTAL_PROVIDER:-podman}"
ENGINE="${HAIRPIN_CONTAINER_ENGINE:-podman}"

log()  { printf '[deploy] %s\n' "$*"; }
warn() { printf '[deploy] WARNING: %s\n' "$*" >&2; }
fail() { printf '[deploy] ERROR: %s\n' "$*" >&2; exit 1; }

# Every manifest here is development-only and several replace cluster
# state by name, so the context is pinned rather than inherited from
# whatever the shell last selected.
KUBECTL=(kubectl --context "${KUBE_CONTEXT}")

# Point a Deployment at an image held only in the cluster's own store.
# The strategic-merge patch is keyed on the container name, so it adds
# or replaces imagePullPolicy without needing to know which.
pin_local_image() {
    local deployment="$1" container="$2" image="$3"
    "${KUBECTL[@]}" -n hairpin set image "deployment/${deployment}" "${container}=${image}"
    "${KUBECTL[@]}" -n hairpin patch deployment "${deployment}" -p \
        "{\"spec\":{\"template\":{\"spec\":{\"containers\":[{\"name\":\"${container}\",\"imagePullPolicy\":\"IfNotPresent\"}]}}}}"
}

# `kind get clusters` is unusable against this podman (see kind-up.sh);
# kind's node container is the reliable signal.
"${ENGINE}" container exists "${CLUSTER_NAME}-control-plane" 2>/dev/null \
    || fail "cluster '${CLUSTER_NAME}' does not exist; run scripts/dev/kind-up.sh"

kubectl config get-contexts -o name | grep -qx "${KUBE_CONTEXT}" \
    || fail "no kubectl context named '${KUBE_CONTEXT}'; set HAIRPIN_KUBE_CONTEXT to override"

workdir="$(mktemp -d -t hairpin-images-XXXXXX)"
archive="${workdir}/hairpin.tar"
billet_archive="${workdir}/billet.tar"
steeplechase_archive="${workdir}/steeplechase.tar"
billet_local=false
steeplechase_local=false
trap 'rm -rf "${workdir}"' EXIT INT TERM

log "building ${IMAGE}..."
"${ENGINE}" build -t "${IMAGE}" -f "${REPO_ROOT}/Containerfile" "${REPO_ROOT}"

log "loading ${IMAGE} into the cluster..."
"${ENGINE}" save --format oci-archive -o "${archive}" "${IMAGE}"
kind load image-archive "${archive}" --name "${CLUSTER_NAME}"

if [ -f "${BILLET_DIR}/Containerfile" ]; then
    log "building ${BILLET_IMAGE} from ${BILLET_DIR}..."
    (cd "${BILLET_DIR}" && "${ENGINE}" build -t "${BILLET_IMAGE}" -f Containerfile .)

    log "loading ${BILLET_IMAGE} into the cluster..."
    "${ENGINE}" save --format oci-archive -o "${billet_archive}" "${BILLET_IMAGE}"
    kind load image-archive "${billet_archive}" --name "${CLUSTER_NAME}"
    billet_local=true
else
    log "no Billet checkout at ${BILLET_DIR}; the published image will be pulled"
fi

# steeplechase is best-effort. Its Dockerfile builds with a Go toolchain
# older than its own go.mod requires, so the build fails until that is
# fixed upstream, and its published image is not pullable — which makes
# an image already in the store worth reusing, since the alternative is
# leaving the Deployment pointed at a tag that cannot be pulled. A run
# whose trace has nowhere to go still completes: OTLP export does not
# block the harness.
if [ -f "${STEEPLECHASE_DIR}/Dockerfile" ]; then
    log "building ${STEEPLECHASE_IMAGE} from ${STEEPLECHASE_DIR}..."
    if (cd "${STEEPLECHASE_DIR}" && "${ENGINE}" build -t "${STEEPLECHASE_IMAGE}" -f Dockerfile .); then
        steeplechase_local=true
    else
        warn "the steeplechase build failed"
    fi
else
    log "no steeplechase checkout at ${STEEPLECHASE_DIR}"
fi
if [ "${steeplechase_local}" = false ] && "${ENGINE}" image exists "${STEEPLECHASE_IMAGE}"; then
    log "reusing the ${STEEPLECHASE_IMAGE} already in the image store"
    steeplechase_local=true
fi
if [ "${steeplechase_local}" = true ]; then
    log "loading ${STEEPLECHASE_IMAGE} into the cluster..."
    "${ENGINE}" save --format oci-archive -o "${steeplechase_archive}" "${STEEPLECHASE_IMAGE}"
    kind load image-archive "${steeplechase_archive}" --name "${CLUSTER_NAME}"
else
    warn "no steeplechase image is available; its Deployment keeps the published" \
         "reference and runs may have no collector to export to"
fi

log "applying manifests..."
"${KUBECTL[@]}" apply \
    -f "${REPO_ROOT}/examples/k8s/namespace.yaml" \
    -f "${REPO_ROOT}/examples/k8s/rbac.yaml" \
    -f "${REPO_ROOT}/examples/k8s/rbac-sandbox.yaml" \
    -f "${REPO_ROOT}/examples/k8s/redis.yaml" \
    -f "${REPO_ROOT}/examples/k8s/billet.yaml" \
    -f "${REPO_ROOT}/examples/k8s/steeplechase.yaml" \
    -f "${REPO_ROOT}/examples/k8s/profiles.yaml" \
    -f "${REPO_ROOT}/examples/k8s/hairpin.yaml"

# The fake provider ships its own hairpin-profiles ConfigMap, replacing
# the reference one so the default profile points at it.
log "applying the development fake provider..."
"${KUBECTL[@]}" -n hairpin create secret generic provider-api-keys \
    --from-literal=FAKE_API_KEY=not-a-real-key \
    --dry-run=client -o yaml | "${KUBECTL[@]}" apply -f -
"${KUBECTL[@]}" apply -f "${SCRIPT_DIR}/fake-provider.yaml"

log "pointing the Deployments at the locally built images..."
pin_local_image hairpin hairpin "${IMAGE}"
if [ "${billet_local}" = true ]; then
    pin_local_image billet billet "${BILLET_IMAGE}"
fi
if [ "${steeplechase_local}" = true ]; then
    pin_local_image steeplechase steeplechase "${STEEPLECHASE_IMAGE}"
fi

log "waiting for rollouts..."
"${KUBECTL[@]}" -n hairpin rollout status deployment/redis --timeout=120s
"${KUBECTL[@]}" -n hairpin rollout status deployment/billet --timeout=120s
# Telemetry is not on the path a smoke test asserts, so a steeplechase
# that cannot pull its image holds nothing else up.
if [ "${steeplechase_local}" = true ]; then
    "${KUBECTL[@]}" -n hairpin rollout status deployment/steeplechase --timeout=120s
elif ! "${KUBECTL[@]}" -n hairpin rollout status deployment/steeplechase --timeout=30s; then
    warn "steeplechase is not ready; runs will export their traces nowhere"
fi
# The provider reads server.py once at start, so a changed ConfigMap
# only takes effect on a fresh Pod.
"${KUBECTL[@]}" -n hairpin rollout restart deployment/fake-provider
"${KUBECTL[@]}" -n hairpin rollout status deployment/fake-provider --timeout=120s
"${KUBECTL[@]}" -n hairpin rollout restart deployment/hairpin
"${KUBECTL[@]}" -n hairpin rollout status deployment/hairpin --timeout=120s

log "done. Next: scripts/dev/smoke-test.sh"
