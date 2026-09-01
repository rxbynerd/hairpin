#!/usr/bin/env bash
# Submit one job against the deployed cluster and assert it ran in a
# sandbox Pod.
#
# The fake provider makes the run call run_command once, so a success
# here means the whole chain held: hairpin created a harness Job, the
# harness dialled the control plane back, created a sandbox Pod, drove a
# shell command through pods/exec, and settled the job terminally.

set -euo pipefail

NAMESPACE="${HAIRPIN_NAMESPACE:-hairpin}"
CLUSTER_NAME="${HAIRPIN_CLUSTER_NAME:-hairpin}"
KUBE_CONTEXT="${HAIRPIN_KUBE_CONTEXT:-kind-${CLUSTER_NAME}}"
PORT="${HAIRPIN_PORT:-8130}"
BASE="http://localhost:${PORT}"

log()  { printf '[smoke] %s\n' "$*"; }
fail() { printf '[smoke] FAIL: %s\n' "$*" >&2; exit 1; }

# Pinned so the probe cannot be answered by whichever cluster the shell
# last selected.
KUBECTL=(kubectl --context "${KUBE_CONTEXT}")

"${KUBECTL[@]}" -n "${NAMESPACE}" port-forward "svc/hairpin" "${PORT}:8130" >/dev/null 2>&1 &
forward_pid=$!
trap 'kill "${forward_pid}" 2>/dev/null || true' EXIT INT TERM

log "waiting for the port-forward..."
for _ in $(seq 1 30); do
    if curl -fsS --max-time 5 "${BASE}/healthz" >/dev/null 2>&1; then break; fi
    sleep 1
done
curl -fsS --max-time 5 "${BASE}/healthz" >/dev/null || fail "hairpin did not become reachable on ${BASE}"

log "submitting..."
job_id="$(curl -fsS --max-time 10 "${BASE}/hairpin.v1.JobService/SubmitJob" \
    -H 'Content-Type: application/json' \
    -d '{"prompt": "probe the sandbox"}' \
    | python3 -c 'import sys, json; print(json.load(sys.stdin)["job"]["id"])')"
log "job ${job_id}"

status=""
for _ in $(seq 1 60); do
    response="$(curl -fsS --max-time 10 "${BASE}/hairpin.v1.JobService/GetJob" \
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

# The fake provider's probe echoes this from inside the sandbox Pod.
printf '%s' "${response}" | grep -q 'Sandbox probe complete' \
    || fail "job succeeded without the sandbox probe's final text: ${response}"

log "job ${job_id} succeeded, sandbox probe reported back"
