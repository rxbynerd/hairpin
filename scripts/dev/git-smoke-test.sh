#!/usr/bin/env bash
# Submit one job through the "git" profile and assert the push actually
# landed in gitea — not just that the job succeeded, which would also
# be true of a broken proxy path the sandbox silently gave up on.
#
# Run after scripts/dev/haybale.sh. GITEA_USER/GITEA_REPO below must
# match the values it seeded (HAIRPIN_GITEA_USER/HAIRPIN_GITEA_REPO, if
# overridden there).

set -euo pipefail

NAMESPACE="${HAIRPIN_NAMESPACE:-hairpin}"
PORT="${HAIRPIN_PORT:-8130}"
GITEA_PORT="${HAIRPIN_GITEA_PORT:-3000}"
BASE="http://localhost:${PORT}"
GITEA_BASE="http://localhost:${GITEA_PORT}"
GITEA_USER="${HAIRPIN_GITEA_USER:-hairpin-dev}"
GITEA_REPO="${HAIRPIN_GITEA_REPO:-e2e-repo}"

log()  { printf '[git-smoke] %s\n' "$*"; }
fail() { printf '[git-smoke] FAIL: %s\n' "$*" >&2; exit 1; }

kubectl -n "${NAMESPACE}" port-forward "svc/hairpin" "${PORT}:8130" >/dev/null 2>&1 &
hairpin_forward_pid=$!
kubectl -n "${NAMESPACE}" port-forward "svc/gitea" "${GITEA_PORT}:3000" >/dev/null 2>&1 &
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

commits_before="$(curl -fsS "${GITEA_BASE}/api/v1/repos/${GITEA_USER}/${GITEA_REPO}/commits?limit=1" \
    | python3 -c 'import sys, json; print(len(json.load(sys.stdin)))' 2>/dev/null || echo 0)"

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
commits_after="$(curl -fsS "${GITEA_BASE}/api/v1/repos/${GITEA_USER}/${GITEA_REPO}/commits?limit=1" \
    | python3 -c 'import sys, json; print(len(json.load(sys.stdin)))')"
[ -n "${commits_after}" ] && [ "${commits_after}" -ge 1 ] \
    || fail "no commits visible on ${GITEA_USER}/${GITEA_REPO} after the run"

curl -fsS "${GITEA_BASE}/api/v1/repos/${GITEA_USER}/${GITEA_REPO}/contents/PROOF.md" >/dev/null \
    || fail "PROOF.md is not present in ${GITEA_USER}/${GITEA_REPO} after the run"

log "job ${job_id} succeeded, push confirmed in gitea (commits before: ${commits_before}, after: ${commits_after})"
