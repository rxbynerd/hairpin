# Hairpin design

Hairpin reverses the client-server direction of
[stirrup](https://github.com/rxbynerd/stirrup). Stirrup's `stirrup job`
is intentionally a *client*: it dials the gRPC address in
`CONTROL_PLANE_ADDR` and asks for work. That is the right shape for
organisations running a full control plane, but it asks a lot from
event-driven callers — a Lambda handling a webhook should not have to
host a bidirectional gRPC server for the lifetime of a run.

Hairpin is that control plane, packaged as a service. A caller submits
a task over a simple request/response API and gets back a job ID. Hairpin
launches a `stirrup job` on its Kubernetes cluster, hands the task across
when the harness dials back in, records everything the harness streams,
and lets the original caller — or anyone else, such as the bundled web
UI — retrieve status, events, and the result by job ID.

The name follows the equestrianism series (stirrup, haybale): a hairpin
is the turn that reverses the direction of travel.

## Flow

```
caller ──SubmitJob──▶ hairpin ──creates──▶ K8s Job (stirrup job)
   ◀─job id──┘           ▲                        │
                         └──── RunTask bidi ──────┘
                              (harness dials CONTROL_PLANE_ADDR,
                               ready.id = CONTROL_PLANE_SESSION_ID
                                        = hairpin job ID)
caller / web UI ──GetJob / WatchJob / CancelJob / AnswerPermission──▶ hairpin ──▶ Redis
```

1. `SubmitJob` validates the task, resolves a RunConfig from a named
   **profile** (a protojson RunConfig template) or an explicit
   `run_config_json`, forces `runId` to the hairpin job ID, persists the
   job in Redis (`queued`), and asks the Launcher to start a harness
   (`launching`).
2. The launcher creates a Kubernetes `batch/v1` Job running `stirrup job`
   with `CONTROL_PLANE_ADDR` pointing back at hairpin and
   `CONTROL_PLANE_SESSION_ID` set to the hairpin job ID
   (`awaiting_harness`).
3. The harness opens the `RunTask` stream and sends `ready`. Hairpin
   correlates via `ready.id`, sends `task_assignment` with the stored
   RunConfig (`running`), and pumps every harness event into a Redis
   stream. Heartbeats update a liveness timestamp.
4. `permission_request` events are persisted as pending approvals;
   `AnswerPermission` (API or UI button) routes the decision onto the
   live stream via the in-process session registry.
5. `done.stop_reason` finalises the job: `success` → `succeeded`,
   `cancelled` → `cancelled`, anything else → `failed` (reason
   preserved verbatim — stirrup adds stop reasons over time). Stream
   closure without `done` marks the job `failed` with a crash note.

## Components

| Package | Responsibility |
|---|---|
| `internal/job` | Job model, statuses, ID generation. Pure types. |
| `internal/store` | `Store` interface + in-memory impl (tests/dev). |
| `internal/store/redisstore` | Redis impl: jobs as hashes, events as capped streams, index as zset, blocking event subscription. |
| `internal/registry` | In-process map of live harness sessions; the only bridge from API handlers to an open `RunTask` stream. |
| `internal/controlplane` | connect-go handler for `stirrup.harness.v1.HarnessService` — correlation, assignment, event pump, permission bridging, terminal handling. |
| `internal/service` | Core operations (Submit/Get/List/Watch/Cancel/Answer) shared by the connect API and the web UI. |
| `internal/api` | connect-go handler for `hairpin.v1.JobService`, a thin shim over `internal/service`. |
| `internal/launcher` | `Launcher` interface; `k8s` (client-go batch/v1 Job) and `process` (local `stirrup job` subprocess, for dev and e2e tests) impls. |
| `internal/web` | Embedded html/template UI: job list, submit form, job detail with SSE live event feed, approve/deny buttons. |
| `internal/config` | Flags/env → Config: listen addr, advertise addr, Redis, launcher selection, profiles dir. |
| `cmd/hairpin` | `hairpin serve`; wires everything onto one h2c listener. |

Both proto services are served by connect-go on a single h2c port, so
`stirrup job`'s grpc-go client, gRPC clients, and plain JSON/HTTP
clients all work against the same listener, and the web UI rides
alongside. The vendored `proto/harness/v1/harness.proto` keeps package
`stirrup.harness.v1` so the wire path
`/stirrup.harness.v1.HarnessService/RunTask` matches what the harness
dials.

## Redis layout

| Key | Type | Contents |
|---|---|---|
| `hairpin:job:<id>` | hash | job fields (status, prompt, runconfig JSON, stop reason, timestamps, last_event_at) |
| `hairpin:job:<id>:events` | stream | harness events (protojson payloads), XADD with MAXLEN ~10000 |
| `hairpin:job:<id>:perms` | hash | pending/answered permission requests keyed by request_id |
| `hairpin:jobs` | zset | job IDs scored by creation time (listing, newest first) |

Job IDs are lowercase ULIDs prefixed `hp-` — sortable, and satisfying
stirrup's run_id constraints (no path separators, `..`, control bytes).
`runId` is always the hairpin job ID.

## Trust posture (v0.1)

Hairpin inherits stirrup v0.1's plaintext, unauthenticated gRPC posture:
run it on a trusted network (cluster-internal Service, mesh mTLS). The
JobService API and web UI carry no authentication yet either — front
them with your ingress's auth. Do not expose either port publicly.

Within that posture, harness streams are still not trusted on job ID
alone: submission mints a per-job bearer token, launchers pass
`<job id>.<token>` as `CONTROL_PLANE_SESSION_ID`, and the control plane
rejects a `ready` whose token does not match (constant-time). Job IDs
are time-ordered ULIDs visible in pod names and URLs; without the token
a neighbouring pod could claim another job's stream and read its
RunConfig. Both connect services cap received messages at 4 MiB,
permission requests are capped per job, the web UI enforces same-origin
on state-changing requests, and shutdown cancels live runs so restarts
do not strand jobs in `running`.

## Deliberately deferred (v1 scope cuts)

- Follow-up turns (`followUpGrace` / `user_response`) — single run per job.
- `sandbox_token_request` — answered with an explicit `is_error` refusal
  so opted-in configs fail fast rather than hang.
- `batch_submission` / `tool_result_request` — not answered; don't
  enable those RunConfig features via hairpin yet.
- Multi-replica hairpin — the session registry is in-process. Scaling
  out needs the control-event bridge moved to Redis pub/sub.
- AuthN/AuthZ on the API and UI.
