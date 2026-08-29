#!/usr/bin/env bash
# Build hairpin, load it into the kind cluster, and apply the reference
# manifests plus the development-only fake provider.
#
# The reference manifests name a published image; this script overrides
# that with the locally built one, which never leaves the cluster's
# image store (hence imagePullPolicy: IfNotPresent).

set -euo pipefail

CLUSTER_NAME="${HAIRPIN_CLUSTER_NAME:-hairpin}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
IMAGE="localhost/hairpin:dev"

export KIND_EXPERIMENTAL_PROVIDER="${KIND_EXPERIMENTAL_PROVIDER:-podman}"
ENGINE="${HAIRPIN_CONTAINER_ENGINE:-podman}"

log()  { printf '[deploy] %s\n' "$*"; }
fail() { printf '[deploy] ERROR: %s\n' "$*" >&2; exit 1; }

# `kind get clusters` is unusable against this podman (see kind-up.sh);
# kind's node container is the reliable signal.
"${ENGINE}" container exists "${CLUSTER_NAME}-control-plane" 2>/dev/null \
    || fail "cluster '${CLUSTER_NAME}' does not exist; run scripts/dev/kind-up.sh"

archive="$(mktemp -t hairpin-image-XXXXXX).tar"
trap 'rm -f "${archive}"' EXIT

log "building ${IMAGE}..."
"${ENGINE}" build -t "${IMAGE}" -f "${REPO_ROOT}/Containerfile" "${REPO_ROOT}"

log "loading ${IMAGE} into the cluster..."
"${ENGINE}" save --format oci-archive -o "${archive}" "${IMAGE}"
kind load image-archive "${archive}" --name "${CLUSTER_NAME}"

log "applying manifests..."
kubectl apply \
    -f "${REPO_ROOT}/examples/k8s/namespace.yaml" \
    -f "${REPO_ROOT}/examples/k8s/rbac.yaml" \
    -f "${REPO_ROOT}/examples/k8s/rbac-sandbox.yaml" \
    -f "${REPO_ROOT}/examples/k8s/redis.yaml" \
    -f "${REPO_ROOT}/examples/k8s/profiles.yaml" \
    -f "${REPO_ROOT}/examples/k8s/hairpin.yaml"

# The fake provider ships its own hairpin-profiles ConfigMap, replacing
# the reference one so the default profile points at it.
log "applying the development fake provider..."
kubectl -n hairpin create secret generic provider-api-keys \
    --from-literal=FAKE_API_KEY=not-a-real-key \
    --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f "${SCRIPT_DIR}/fake-provider.yaml"

log "pointing the Deployment at the locally built image..."
kubectl -n hairpin set image deployment/hairpin "hairpin=${IMAGE}"
kubectl -n hairpin patch deployment hairpin --type=json \
    -p '[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]' \
    2>/dev/null \
  || kubectl -n hairpin patch deployment hairpin --type=json \
    -p '[{"op":"add","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]'

log "waiting for rollouts..."
kubectl -n hairpin rollout status deployment/redis --timeout=120s
kubectl -n hairpin rollout status deployment/fake-provider --timeout=120s
kubectl -n hairpin rollout restart deployment/hairpin
kubectl -n hairpin rollout status deployment/hairpin --timeout=120s

log "done. Next: scripts/dev/smoke-test.sh"
