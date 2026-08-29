#!/usr/bin/env bash
# Wire a real OpenRouter-backed profile into the running development
# cluster: read the API key from 1Password, add it to the
# provider-api-keys Secret, add an "openrouter" profile to the
# hairpin-profiles ConfigMap, and restart hairpin to load it.
#
# Run after scripts/dev/deploy.sh — deploy.sh recreates the Secret and
# ConfigMap without these additions, so this script must be re-run after
# every deploy.
#
# Usage: openrouter.sh [op-ref]
#   op-ref  1Password secret reference for the key, e.g.
#           "op://Private/<item-id>/credential". Defaults to
#           $HAIRPIN_OPENROUTER_OP_REF.

set -euo pipefail

NAMESPACE="${HAIRPIN_NAMESPACE:-hairpin}"
MODEL="${HAIRPIN_OPENROUTER_MODEL:-google/gemini-3.7-flash}"
OP_REF="${1:-${HAIRPIN_OPENROUTER_OP_REF:-}}"

log()  { printf '[openrouter] %s\n' "$*"; }
fail() { printf '[openrouter] ERROR: %s\n' "$*" >&2; exit 1; }

[ -n "${OP_REF}" ] \
    || fail "no 1Password reference: pass one as the first argument or set HAIRPIN_OPENROUTER_OP_REF"

log "reading the API key from 1Password..."
key="$(op read "${OP_REF}")"
[ -n "${key}" ] || fail "op read returned an empty key"

log "adding OPENROUTER_API_KEY to the provider-api-keys Secret..."
kubectl -n "${NAMESPACE}" patch secret provider-api-keys --type=merge \
    -p "$(python3 -c 'import json, sys; print(json.dumps({"stringData": {"OPENROUTER_API_KEY": sys.argv[1]}}))' "${key}")"

log "adding the openrouter profile (model ${MODEL})..."
profile="$(python3 - "${MODEL}" <<'EOF'
import json, sys
print(json.dumps({"data": {"openrouter.json": json.dumps({
    "mode": "execution",
    "provider": {
        "type": "openai-compatible",
        "baseUrl": "https://openrouter.ai/api/v1",
        "apiKeyRef": "secret://OPENROUTER_API_KEY",
    },
    "modelRouter": {"type": "static", "model": sys.argv[1]},
    "executor": {"type": "k8s", "network": {"mode": "none"}},
    "permissionPolicy": {"type": "allow-all"},
    "maxTurns": 10,
    "timeout": 600,
}, indent=2)}}))
EOF
)"
kubectl -n "${NAMESPACE}" patch configmap hairpin-profiles --type=merge -p "${profile}"

# Profiles are loaded once at startup, so a restart is required.
log "restarting hairpin..."
kubectl -n "${NAMESPACE}" rollout restart deployment/hairpin
kubectl -n "${NAMESPACE}" rollout status deployment/hairpin --timeout=120s

log 'done. Submit with {"profile": "openrouter", "prompt": "..."}'
