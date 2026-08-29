#!/usr/bin/env bash
# Build+load haybale, deploy an in-cluster gitea and bootstrap it
# non-interactively, wire the "git" profile at both, and restart
# hairpin so it picks the profile up.
#
# Run after scripts/dev/deploy.sh — deploy.sh already generates and
# installs the sandbox-token keypair (hairpin-sandbox-token-key Secret,
# haybale-jwks ConfigMap) hairpin and haybale share; this script only
# consumes it, and re-running deploy.sh (which regenerates that pair)
# means re-running this script too.
#
# haybale itself is a local, read-only reference checkout — this script
# only builds a container image from it, never modifies it.

set -euo pipefail

NAMESPACE="${HAIRPIN_NAMESPACE:-hairpin}"
CLUSTER_NAME="${HAIRPIN_CLUSTER_NAME:-hairpin}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
HAYBALE_DIR="${HAYBALE_DIR:-${HOME}/Developer/haybale}"
IMAGE="localhost/haybale:dev"

GITEA_USER="${HAIRPIN_GITEA_USER:-hairpin-dev}"
GITEA_REPO="${HAIRPIN_GITEA_REPO:-e2e-repo}"
GITEA_TOKEN_NAME="hairpin-dev-$(date +%s)"

export KIND_EXPERIMENTAL_PROVIDER="${KIND_EXPERIMENTAL_PROVIDER:-podman}"
ENGINE="${HAIRPIN_CONTAINER_ENGINE:-podman}"

log()  { printf '[haybale] %s\n' "$*"; }
fail() { printf '[haybale] ERROR: %s\n' "$*" >&2; exit 1; }

"${ENGINE}" container exists "${CLUSTER_NAME}-control-plane" 2>/dev/null \
    || fail "cluster '${CLUSTER_NAME}' does not exist; run scripts/dev/kind-up.sh"
kubectl -n "${NAMESPACE}" get deployment hairpin >/dev/null 2>&1 \
    || fail "hairpin is not deployed in namespace '${NAMESPACE}'; run scripts/dev/deploy.sh first"
[ -d "${HAYBALE_DIR}" ] \
    || fail "no haybale checkout at ${HAYBALE_DIR}; set HAYBALE_DIR to override"

archive="$(mktemp -t haybale-image-XXXXXX).tar"
trap 'rm -f "${archive}"' EXIT

log "building ${IMAGE} from ${HAYBALE_DIR}..."
"${ENGINE}" build -t "${IMAGE}" "${HAYBALE_DIR}"

log "loading ${IMAGE} into the cluster..."
"${ENGINE}" save --format oci-archive -o "${archive}" "${IMAGE}"
kind load image-archive "${archive}" --name "${CLUSTER_NAME}"

log "deploying gitea..."
kubectl apply -f "${SCRIPT_DIR}/gitea.yaml"
kubectl -n "${NAMESPACE}" rollout status deployment/gitea --timeout=180s

log "bootstrapping gitea (admin user, access token, seed repo)..."
kubectl -n "${NAMESPACE}" port-forward svc/gitea 3000:3000 >/dev/null 2>&1 &
gitea_forward_pid=$!
trap 'kill "${gitea_forward_pid}" 2>/dev/null || true; rm -f "${archive}"' EXIT

for _ in $(seq 1 30); do
    if curl -fsS http://localhost:3000/api/v1/version >/dev/null 2>&1; then break; fi
    sleep 1
done
curl -fsS http://localhost:3000/api/v1/version >/dev/null \
    || fail "gitea did not become reachable on http://localhost:3000"

gitea_password="$(openssl rand -hex 16 2>/dev/null || head -c16 /dev/urandom | od -An -tx1 | tr -d ' \n')"

# Fails harmlessly on a rerun (user already exists); the CLI's exact
# arguments are per gitea's documented Docker-image bootstrap recipe as
# of the pinned image tag in gitea.yaml — re-check against the image's
# `gitea admin user create --help` if this ever drifts.
kubectl -n "${NAMESPACE}" exec deploy/gitea -- su-exec git gitea admin user create \
    --username "${GITEA_USER}" --password "${gitea_password}" \
    --email "${GITEA_USER}@example.com" --admin --must-change-password=false \
    || log "admin user create failed (may already exist); continuing"

# --raw prints only the token, nothing else, for exactly this kind of
# scripted consumption; a unique --token-name per run sidesteps gitea's
# "token name already used" rejection on a rerun.
token="$(kubectl -n "${NAMESPACE}" exec deploy/gitea -- su-exec git gitea admin user generate-access-token \
    --username "${GITEA_USER}" --token-name "${GITEA_TOKEN_NAME}" \
    --scopes read:repository,write:repository --raw)"
[ -n "${token}" ] || fail "gitea did not return an access token"

curl -fsS -u "${GITEA_USER}:${gitea_password}" \
    -H 'Content-Type: application/json' \
    -d "{\"name\": \"${GITEA_REPO}\", \"private\": false, \"auto_init\": true}" \
    http://localhost:3000/api/v1/user/repos >/dev/null \
    || log "repo create failed (may already exist); continuing"

kill "${gitea_forward_pid}" 2>/dev/null || true
trap 'rm -f "${archive}"' EXIT

log "deploying haybale..."
kubectl apply -f "${REPO_ROOT}/examples/k8s/haybale.yaml"

# haybale.yaml bundles its own placeholder haybale-gitea-token Secret
# (REPLACE_ME) so `kubectl apply -f examples/k8s/` has something to
# mount outside this script; applying it here would clobber the real
# token, so the real one is written after, not before.
log "recording the gitea token in the haybale-gitea-token Secret..."
kubectl -n "${NAMESPACE}" create secret generic haybale-gitea-token \
    --from-literal=HAYBALE_GITEA_TOKEN="${token}" \
    --dry-run=client -o yaml | kubectl apply -f -

kubectl -n "${NAMESPACE}" set image deployment/haybale "haybale=${IMAGE}"
kubectl -n "${NAMESPACE}" patch deployment haybale --type=json \
    -p '[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]' \
    2>/dev/null \
  || kubectl -n "${NAMESPACE}" patch deployment haybale --type=json \
    -p '[{"op":"add","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}]'
kubectl -n "${NAMESPACE}" rollout restart deployment/haybale
kubectl -n "${NAMESPACE}" rollout status deployment/haybale --timeout=180s

log "deploying the git-probe fake provider..."
kubectl apply -f "${SCRIPT_DIR}/fake-provider-git.yaml"
kubectl -n "${NAMESPACE}" rollout status deployment/fake-provider-git --timeout=120s

log "adding the git profile (repo: gitea/${GITEA_USER}/${GITEA_REPO})..."
profile="$(python3 - <<'EOF'
import json
print(json.dumps({"data": {"git.json": json.dumps({
    "mode": "execution",
    "provider": {
        "type": "openai-compatible",
        "baseUrl": "http://fake-provider-git.hairpin.svc:8080/v1",
        "apiKeyRef": "secret://FAKE_API_KEY",
    },
    "modelRouter": {"type": "static", "model": "fake-model"},
    "executor": {
        "type": "k8s",
        "network": {"mode": "none"},
        "sandboxIdentity": {"source": "control-plane", "audience": "https://haybale.hairpin.svc"},
        "gitProxy": {"url": "http://haybale.hairpin.svc:8466", "hosts": ["gitea"]},
    },
    "permissionPolicy": {"type": "allow-all"},
    "maxTurns": 6,
    "timeout": 300,
}, indent=2)}}))
EOF
)"
kubectl -n "${NAMESPACE}" patch configmap hairpin-profiles --type=merge -p "${profile}"

# Profiles are loaded once at startup, so a restart is required.
log "restarting hairpin..."
kubectl -n "${NAMESPACE}" rollout restart deployment/hairpin
kubectl -n "${NAMESPACE}" rollout status deployment/hairpin --timeout=120s

log "done. Next: scripts/dev/git-smoke-test.sh (or submit with {\"profile\": \"git\", \"repoScope\": [\"gitea/${GITEA_USER}/${GITEA_REPO}\"], \"prompt\": \"...\"})"
