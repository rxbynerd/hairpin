#!/usr/bin/env bash
# Add a GitHub App upstream to the running development cluster's haybale:
# store the App's private key in the haybale-github-app-key Secret,
# rewrite haybale-config with a github.com upstream alongside the
# development gitea one, widen haybale-policy to the App's owner, and
# restart haybale.
#
# Run after scripts/dev/haybale.sh. A rerun of that script leaves these
# additions in place (it applies examples/k8s/haybale.yaml, whose
# ConfigMaps this script replaces) only until the next
# `kubectl apply` of the manifest, so re-run this script after it.
#
# Usage: haybale-github.sh
#   HAIRPIN_GITHUB_APP_ID    the App's numeric ID (required)
#   HAIRPIN_GITHUB_APP_KEY   path to the App's PEM private key (required)
#   HAIRPIN_GITHUB_OWNER     the account whose repos the policy allows
#                            (required; the App must be installed there)

set -euo pipefail

NAMESPACE="${HAIRPIN_NAMESPACE:-hairpin}"
CLUSTER_NAME="${HAIRPIN_CLUSTER_NAME:-hairpin}"
KUBE_CONTEXT="${HAIRPIN_KUBE_CONTEXT:-kind-${CLUSTER_NAME}}"
APP_ID="${HAIRPIN_GITHUB_APP_ID:-}"
APP_KEY="${HAIRPIN_GITHUB_APP_KEY:-}"
OWNER="${HAIRPIN_GITHUB_OWNER:-}"

log()  { printf '[haybale-github] %s\n' "$*"; }
fail() { printf '[haybale-github] ERROR: %s\n' "$*" >&2; exit 1; }

KUBECTL=(kubectl --context "${KUBE_CONTEXT}")

[ -n "${APP_ID}" ] && [ -n "${APP_KEY}" ] && [ -n "${OWNER}" ] \
    || fail "set HAIRPIN_GITHUB_APP_ID, HAIRPIN_GITHUB_APP_KEY, and HAIRPIN_GITHUB_OWNER"
[[ "${APP_ID}" =~ ^[0-9]+$ ]] || fail "HAIRPIN_GITHUB_APP_ID must be numeric: ${APP_ID}"
[ -r "${APP_KEY}" ] || fail "cannot read the private key at ${APP_KEY}"
"${KUBECTL[@]}" -n "${NAMESPACE}" get deployment haybale >/dev/null 2>&1 \
    || fail "haybale is not deployed in namespace '${NAMESPACE}'; run scripts/dev/haybale.sh first"

workdir="$(mktemp -d -t haybale-github-XXXXXX)"
trap 'rm -rf "${workdir}"' EXIT INT TERM

log "storing the App private key in the haybale-github-app-key Secret..."
"${KUBECTL[@]}" -n "${NAMESPACE}" create secret generic haybale-github-app-key \
    --from-file=private-key.pem="${APP_KEY}" \
    --dry-run=client -o yaml | "${KUBECTL[@]}" apply -f -

# The gitea upstream and rule are kept verbatim so git-smoke-test keeps
# working beside the GitHub one.
log "rewriting haybale-config with a github.com upstream (App ${APP_ID})..."
cat > "${workdir}/haybale.yaml" <<EOF
listen: ":8466"
logLevel: info
drainTimeout: 30s

identity:
  type: jwt
  issuers:
    - issuer: https://hairpin.hairpin.svc
      jwksFile: /etc/haybale/jwks/jwks.json
      algorithms: [ES256]
      audiences: [https://haybale.hairpin.svc]
      typ: at+jwt
      identityTemplate: "{sub}"
      repoScopeClaim: haybale.dev/repos

policy:
  path: /etc/haybale/policy/policy.yaml

upstreams:
  - host: gitea
    baseURL: http://gitea.hairpin.svc:3000
    credential:
      type: static
      username: hairpin-dev
      tokenEnv: HAYBALE_GITEA_TOKEN

  - host: github.com
    baseURL: https://github.com
    credential:
      type: github-app
      appID: ${APP_ID}
      privateKeyPath: /etc/haybale/github-app/github-app.pem
EOF
"${KUBECTL[@]}" -n "${NAMESPACE}" create configmap haybale-config \
    --from-file=haybale.yaml="${workdir}/haybale.yaml" \
    --dry-run=client -o yaml | "${KUBECTL[@]}" apply -f -

log "widening haybale-policy to github.com/${OWNER}/*..."
cat > "${workdir}/policy.yaml" <<EOF
rules:
  - identities: ["hp-*"]
    repos: ["gitea/*/*"]
    permissions: [read, write]
  - identities: ["hp-*"]
    repos: ["github.com/${OWNER}/*"]
    permissions: [read, write]
EOF
"${KUBECTL[@]}" -n "${NAMESPACE}" create configmap haybale-policy \
    --from-file=policy.yaml="${workdir}/policy.yaml" \
    --dry-run=client -o yaml | "${KUBECTL[@]}" apply -f -

# haybale reads its config, policy, and key once at startup.
log "restarting haybale..."
"${KUBECTL[@]}" -n "${NAMESPACE}" rollout restart deployment/haybale
"${KUBECTL[@]}" -n "${NAMESPACE}" rollout status deployment/haybale --timeout=180s

log "done. Submit with {\"profile\": \"<name>-git\", \"repoScope\": [\"github.com/${OWNER}/<repo>\"], \"prompt\": \"...\"}"
