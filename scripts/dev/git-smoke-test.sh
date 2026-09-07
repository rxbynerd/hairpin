#!/usr/bin/env bash
# Submit one job through the "git" profile and assert the push actually
# landed in gitea — not just that the job succeeded, which would also
# be true of a broken proxy path the sandbox silently gave up on.
#
# Run after scripts/dev/haybale.sh. The gitea user and repo below are
# the ones it seeds, and are fixed on both sides.

set -euo pipefail

NAMESPACE="${HAIRPIN_NAMESPACE:-hairpin}"
CLUSTER_NAME="${HAIRPIN_CLUSTER_NAME:-hairpin}"
KUBE_CONTEXT="${HAIRPIN_KUBE_CONTEXT:-kind-${CLUSTER_NAME}}"
PORT="${HAIRPIN_PORT:-8130}"
GITEA_PORT="${HAIRPIN_GITEA_PORT:-3000}"
BASE="http://localhost:${PORT}"
GITEA_BASE="http://localhost:${GITEA_PORT}"
GITEA_USER="hairpin-dev"
GITEA_REPO="e2e-repo"

log()  { printf '[git-smoke] %s\n' "$*"; }
fail() { printf '[git-smoke] FAIL: %s\n' "$*" >&2; exit 1; }

# Pinned so the probe cannot be answered by whichever cluster the shell
# last selected.
KUBECTL=(kubectl --context "${KUBE_CONTEXT}")

"${KUBECTL[@]}" -n "${NAMESPACE}" port-forward "svc/hairpin" "${PORT}:8130" >/dev/null 2>&1 &
hairpin_forward_pid=$!
"${KUBECTL[@]}" -n "${NAMESPACE}" port-forward "svc/gitea" "${GITEA_PORT}:3000" >/dev/null 2>&1 &
gitea_forward_pid=$!
trap 'kill "${hairpin_forward_pid}" "${gitea_forward_pid}" 2>/dev/null || true' EXIT

log "waiting for the port-forwards..."
for _ in $(seq 1 30); do
    if curl -fsS "${BASE}/healthz" >/dev/null 2>&1; then break; fi
    sleep 1
done
curl -fsS "${BASE}/healthz" >/dev/null || fail "hairpin did not become reachable on ${BASE}"
for _ in $(seq 1 30); do
    if curl -fsS "${GITEA_BASE}/api/v1/version" >/dev/null 2>&1; then break; fi
    sleep 1
done
curl -fsS "${GITEA_BASE}/api/v1/version" >/dev/null || fail "gitea did not become reachable on ${GITEA_BASE}"

# The push is only observable as a move: the repo is seeded with
# auto_init and PROOF.md survives a rerun, so the head sha before the
# run is what a later comparison has to beat.
branch_head() {
    curl -fsS "${GITEA_BASE}/api/v1/repos/${GITEA_USER}/${GITEA_REPO}/branches/main" \
        | python3 -c 'import sys, json; print(json.load(sys.stdin)["commit"]["id"])'
}
sha_before="$(branch_head)" \
    || fail "cannot read ${GITEA_USER}/${GITEA_REPO} main before the run; is scripts/dev/haybale.sh done?"

log "submitting..."
job_id="$(curl -fsS "${BASE}/hairpin.v1.JobService/SubmitJob" \
    -H 'Content-Type: application/json' \
    -d "{\"prompt\": \"prove the git proxy\", \"profile\": \"git\", \"repoScope\": [\"gitea/${GITEA_USER}/${GITEA_REPO}\"]}" \
    | python3 -c 'import sys, json; print(json.load(sys.stdin)["job"]["id"])')"
log "job ${job_id}"

status=""
for _ in $(seq 1 60); do
    response="$(curl -fsS "${BASE}/hairpin.v1.JobService/GetJob" \
        -H 'Content-Type: application/json' -d "{\"id\": \"${job_id}\"}")"
    status="$(printf '%s' "${response}" \
        | python3 -c 'import sys, json; print(json.load(sys.stdin)["job"]["status"])')"
    case "${status}" in
        JOB_STATUS_SUCCEEDED|JOB_STATUS_FAILED|JOB_STATUS_CANCELLED) break ;;
    esac
    sleep 2
done

[ "${status}" = "JOB_STATUS_SUCCEEDED" ] \
    || fail "job ${job_id} ended ${status:-<none>}: ${response:-<no response>}"

# fake-provider-git echoes this once its clone/commit/push chain
# returns 0 — a real assertion, since a failing git command in the
# run_command chain (e.g. "&&"-short-circuited by a proxy auth failure)
# would never reach this final turn.
printf '%s' "${response}" | grep -q 'Git proxy probe complete' \
    || fail "job succeeded without the git probe's final text: ${response}"

log "verifying the push landed in gitea..."
sha_after="$(branch_head)" \
    || fail "cannot read ${GITEA_USER}/${GITEA_REPO} main after the run"
[ "${sha_after}" != "${sha_before}" ] \
    || fail "${GITEA_USER}/${GITEA_REPO} main is still ${sha_before}: the push did not land"

log "job ${job_id} succeeded, push confirmed in gitea (main ${sha_before} -> ${sha_after})"
