#!/usr/bin/env bash
# Submit two jobs against the deployed cluster and assert the second
# recalls what the first saved.
#
# The fake provider makes every run search memory, save a memory naming
# its own prompt, probe the sandbox, and close by quoting the search
# result back. Job B's final text therefore carries job A's memory only
# if the whole chain held: the harness raised both tool calls up the
# RunTask stream, hairpin proxied them to Billet, and Billet kept the
# record between two separate runs.
#
# The direct query against Billet at the end separates the two ways
# that can fail — a record Billet never received from one hairpin did
# not read back.

set -euo pipefail

NAMESPACE="${HAIRPIN_NAMESPACE:-hairpin}"
PORT="${HAIRPIN_PORT:-8130}"
BILLET_PORT="${BILLET_PORT:-8141}"
BASE="http://localhost:${PORT}"
BILLET_BASE="http://localhost:${BILLET_PORT}"

log()  { printf '[memory-smoke] %s\n' "$*"; }
fail() { printf '[memory-smoke] FAIL: %s\n' "$*" >&2; exit 1; }

# A nonce keeps each invocation's memories distinguishable from those
# left in Billet by earlier runs.
nonce="$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"

submit() {
    curl -fsS --max-time 10 "${BASE}/hairpin.v1.JobService/SubmitJob" \
        -H 'Content-Type: application/json' \
        -d "{\"prompt\": \"$1\"}" \
        | python3 -c 'import sys, json; print(json.load(sys.stdin)["job"]["id"])'
}

# Poll one job to a terminal status, then emit its GetJob response.
await() {
    local job_id="$1"
    local response status=""
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
    printf '%s' "${response}"
}

kubectl -n "${NAMESPACE}" port-forward "svc/hairpin" "${PORT}:8130" >/dev/null 2>&1 &
forward_pid=$!
kubectl -n "${NAMESPACE}" port-forward "svc/billet" "${BILLET_PORT}:8141" >/dev/null 2>&1 &
billet_forward_pid=$!
trap 'kill "${forward_pid}" "${billet_forward_pid}" 2>/dev/null || true' EXIT

log "waiting for the port-forward..."
for _ in $(seq 1 30); do
    if curl -fsS --max-time 5 "${BASE}/healthz" >/dev/null 2>&1; then break; fi
    sleep 1
done
curl -fsS --max-time 5 "${BASE}/healthz" >/dev/null \
    || fail "hairpin did not become reachable on ${BASE}"

log "submitting the job that saves (nonce ${nonce})..."
job_a="$(submit "memory-smoke ${nonce}")"
log "job ${job_a}"
await "${job_a}" >/dev/null

log "submitting the job that recalls..."
job_b="$(submit "memory-smoke recall ${nonce}")"
log "job ${job_b}"
response_b="$(await "${job_b}")"

recalled="$(printf '%s' "${response_b}" | python3 -c \
    'import sys, json; j = json.load(sys.stdin)["job"]; print(j.get("finalText", j.get("final_text", "")))')"
expected="Learned: memory-smoke ${nonce}"
printf '%s' "${recalled}" | grep -qF "${expected}" \
    || fail "job ${job_b} did not recall what job ${job_a} saved (wanted '${expected}'): ${recalled:-<empty>}"

log "job ${job_b} recalled job ${job_a}'s memory"

log "querying Billet directly..."
records=""
for _ in $(seq 1 30); do
    records="$(curl -fsS --max-time 5 \
        -H 'Content-Type: application/json' \
        -d "{\"query\":\"memory-smoke ${nonce}\",\"limit\":5}" \
        "${BILLET_BASE}/billet.v1.MemoryService/SearchMemory" 2>/dev/null)" && break
    sleep 1
done
[ -n "${records}" ] || fail "Billet did not become reachable on ${BILLET_BASE}"

# Billet applies no score threshold: an unmatched query still returns
# up to `limit` records, so a non-empty result proves nothing. Only a
# record whose content carries the nonce does.
hit="$(printf '%s' "${records}" | python3 -c '
import sys, json
nonce = sys.argv[1]
for record in json.load(sys.stdin).get("records", []):
    if nonce in record.get("content", ""):
        print(record["content"])
        break
' "${nonce}")"
[ -n "${hit}" ] || fail "Billet holds no record mentioning ${nonce}: ${records}"

log "Billet holds: ${hit}"
log "memory shared between jobs ${job_a} and ${job_b}"
