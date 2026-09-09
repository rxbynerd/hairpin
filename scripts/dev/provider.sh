#!/usr/bin/env bash
# Wire a real openai-compatible provider into the running development
# cluster: add its API key to the provider-api-keys Secret, add a
# "<name>" profile and a "<name>-git" profile (the same run with a
# haybale-proxied git identity) to the hairpin-profiles ConfigMap, and
# restart hairpin to load them.
#
# Both profiles use executor.network.mode "allowlist" through the
# egress proxy examples/k8s/egress-proxy.yaml deploys, which is the only
# network mode under which a sandbox on a NetworkPolicy-enforcing CNI
# can reach haybale.
#
# Run after scripts/dev/deploy.sh — deploy.sh recreates the Secret and
# ConfigMap without these additions, so this script must be re-run after
# every deploy.
#
# Usage: provider.sh <name> <base-url> <model> [op-ref]
#   name      profile name and Secret key stem: "cerebras" stores the
#             key as CEREBRAS_API_KEY.
#   base-url  the provider's OpenAI-compatible base URL.
#   model     the model ID to pin in the profile.
#   op-ref    1Password secret reference for the key, e.g.
#             "op://Private/<item>/credential". Not needed when
#             HAIRPIN_PROVIDER_API_KEY is already set in the environment,
#             which keeps an unattended run clear of 1Password prompts.

set -euo pipefail

NAMESPACE="${HAIRPIN_NAMESPACE:-hairpin}"
CLUSTER_NAME="${HAIRPIN_CLUSTER_NAME:-hairpin}"
KUBE_CONTEXT="${HAIRPIN_KUBE_CONTEXT:-kind-${CLUSTER_NAME}}"
NAME="${1:-}"
BASE_URL="${2:-}"
MODEL="${3:-}"
OP_REF="${4:-}"
GIT_HOSTS="${HAIRPIN_PROVIDER_GIT_HOSTS:-github.com}"
EGRESS_PROXY_URL="${HAIRPIN_EGRESS_PROXY_URL:-http://stirrup-egress-proxy.hairpin-sandboxes.svc:8080}"
HAYBALE_HOSTPORT="${HAIRPIN_HAYBALE_HOSTPORT:-haybale.hairpin.svc:8466}"

log()  { printf '[provider] %s\n' "$*"; }
fail() { printf '[provider] ERROR: %s\n' "$*" >&2; exit 1; }

KUBECTL=(kubectl --context "${KUBE_CONTEXT}")

[ -n "${NAME}" ] && [ -n "${BASE_URL}" ] && [ -n "${MODEL}" ] \
    || fail "usage: provider.sh <name> <base-url> <model> [op-ref]"
[[ "${NAME}" =~ ^[a-z][a-z0-9-]*$ ]] \
    || fail "name must be lower-case letters, digits, and hyphens: ${NAME}"

key="${HAIRPIN_PROVIDER_API_KEY:-}"
if [ -z "${key}" ]; then
    [ -n "${OP_REF}" ] \
        || fail "no API key: set HAIRPIN_PROVIDER_API_KEY or pass a 1Password reference"
    log "reading the API key from 1Password..."
    key="$(op read "${OP_REF}")"
fi
[ -n "${key}" ] || fail "the API key is empty"

key_name="$(printf '%s' "${NAME}" | tr 'a-z-' 'A-Z_')_API_KEY"

log "adding ${key_name} to the provider-api-keys Secret..."
"${KUBECTL[@]}" -n "${NAMESPACE}" patch secret provider-api-keys --type=merge \
    -p "$(python3 -c 'import json, sys; print(json.dumps({"stringData": {sys.argv[1]: sys.argv[2]}}))' "${key_name}" "${key}")"

log "adding the ${NAME} and ${NAME}-git profiles (model ${MODEL})..."
profiles="$(python3 - "${NAME}" "${BASE_URL}" "${MODEL}" "${key_name}" "${GIT_HOSTS}" "${EGRESS_PROXY_URL}" "${HAYBALE_HOSTPORT}" <<'EOF'
import json, sys
name, base_url, model, key_name, git_hosts, proxy_url, haybale = sys.argv[1:8]

memory_tools = [
    {
        "name": "search_memory",
        "description": "Search knowledge saved by earlier sessions. Call this before starting work on a task.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "query": {"type": "string"},
                "limit": {"type": "integer", "description": "Maximum records to return (default 5, max 100)."},
            },
            "required": ["query"],
        },
    },
    {
        "name": "save_memory",
        "description": "Save a fact or outcome that a future session would benefit from knowing.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "content": {"type": "string"},
                "kind": {"type": "string", "enum": ["event", "fact"]},
            },
            "required": ["content"],
        },
    },
]

def profile(with_git):
    executor = {
        "type": "k8s",
        "network": {"mode": "allowlist", "allowlist": [haybale]},
        "k8sEgressProxyUrl": proxy_url,
    }
    if with_git:
        executor["sandboxIdentity"] = {"source": "control-plane", "audience": "https://haybale.hairpin.svc"}
        executor["gitProxy"] = {"url": "http://" + haybale, "hosts": git_hosts.split(",")}
    return {
        "mode": "execution",
        "provider": {"type": "openai-compatible", "baseUrl": base_url, "apiKeyRef": "secret://" + key_name},
        "modelRouter": {"type": "static", "model": model},
        "executor": executor,
        "permissionPolicy": {"type": "allow-all"},
        "tools": {"controlPlane": memory_tools},
        "maxTurns": 40,
        "timeout": 1200,
    }

print(json.dumps({"data": {
    name + ".json": json.dumps(profile(False), indent=2),
    name + "-git.json": json.dumps(profile(True), indent=2),
}}))
EOF
)"
"${KUBECTL[@]}" -n "${NAMESPACE}" patch configmap hairpin-profiles --type=merge -p "${profiles}"

# Profiles are loaded once at startup, so a restart is required.
log "restarting hairpin..."
"${KUBECTL[@]}" -n "${NAMESPACE}" rollout restart deployment/hairpin
"${KUBECTL[@]}" -n "${NAMESPACE}" rollout status deployment/hairpin --timeout=120s

log "done. Submit with {\"profile\": \"${NAME}\", \"prompt\": \"...\"} or {\"profile\": \"${NAME}-git\", \"repoScope\": [\"${GIT_HOSTS%%,*}/<owner>/<repo>\"], \"prompt\": \"...\"}"
