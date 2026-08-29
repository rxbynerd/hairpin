# Hairpin design

Hairpin provides a service control plane for
[stirrup](https://github.com/rxbynerd/stirrup). Stirrup's `stirrup job`
is a client: it dials the gRPC address in `CONTROL_PLANE_ADDR` and asks
for work. Hairpin owns that long-lived stream so request/response
callers do not need to host a bidirectional gRPC server for each run.

Hairpin is that control plane, packaged as a service. A caller submits
a task over a simple request/response API and gets back a job ID. Hairpin
launches a `stirrup job` on its Kubernetes cluster, hands the task across
when the harness dials back in, records everything the harness streams,
and lets the original caller — or anyone else, such as the bundled web
UI — retrieve status, events, and the result by job ID.

The name follows the equestrianism series (stirrup, haybale, billet): a
hairpin is the turn that reverses the direction of travel.

## Flow

```
caller ──SubmitJob──▶ hairpin ──creates──▶ K8s Job (stirrup job)
   ◀─job id──┘           ▲                        │
                         └──── RunTask bidi ──────┘
                              (harness dials CONTROL_PLANE_ADDR,
                               ready.id = CONTROL_PLANE_SESSION_ID
                                        = "<job id>.<session token>")
caller / web UI ──GetJob / WatchJob / CancelJob / AnswerPermission──▶ hairpin ──▶ Redis
harness ──tool_result_request──▶ hairpin ──SearchMemory / SaveMemory──▶ billet
```

1. `SubmitJob` validates the task, resolves a RunConfig from a named
   **profile** (a protojson RunConfig template) or an explicit
   `run_config_json`, forces `runId` to the hairpin job ID, refuses a
   `tools.controlPlane` declaration hairpin cannot answer, persists the
   job in Redis (`queued`), and asks the Launcher to start a harness
   (`launching`).
2. The launcher creates a Kubernetes `batch/v1` Job running `stirrup job`
   with `CONTROL_PLANE_ADDR` pointing back at hairpin and
   `CONTROL_PLANE_SESSION_ID` set to the job's session string —
   `<job id>.<bearer token>` (`awaiting_harness`).
3. The harness opens the `RunTask` stream and sends `ready`. Hairpin
   correlates via `ready.id`, sends `task_assignment` with the stored
   RunConfig (`running`), and pumps every harness event into a Redis
   stream. Heartbeats update a liveness timestamp.
4. `permission_request` events are persisted as pending approvals;
   `AnswerPermission` (API or UI button) routes the decision onto the
   live stream via the in-process session registry.
   `tool_result_request` events for the memory tools the run declared
   are proxied to Billet and answered on the same stream with
   `tool_result_response`; both are recorded on the timeline
   ([`docs/memory.md`](memory.md)).
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
| `internal/controlplane` | connect-go handler for `stirrup.harness.v1.HarnessService` — correlation, assignment, event pump, permission bridging, memory-call admission and fulfilment, terminal handling. |
| `internal/memory` | Billet client (connect-go, plaintext gRPC) and the `search_memory` / `save_memory` tool contracts: input validation, Billet's request limits, and the error policy that decides what a model is told. |
| `internal/service` | Core operations (Submit/Get/List/Watch/Cancel/Answer) shared by the connect API and the web UI. |
| `internal/api` | connect-go handler for `hairpin.v1.JobService`, a thin shim over `internal/service`. |
| `internal/launcher` | `Launcher` interface; a client-go `batch/v1` Job impl and `None` for harnesses started out-of-band. |
| `internal/web` | Embedded html/template UI: job list, submit form, job detail with SSE live event feed, approve/deny buttons. |
| `internal/telemetry` | OpenTelemetry pipeline (OTLP or stdout, off by default) and the recorder for hairpin's own spans and metrics; owns metric-attribute cardinality. |
| `internal/config` | Flags/env → Config: listen addr, advertise addr, Redis, profiles dir, harness Job settings, telemetry export, and the sandbox coordinates and trace-emitter endpoint submitted RunConfigs inherit. |
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

## Trust posture

Hairpin serves plaintext h2c on one listener. The JobService API and web
UI do not authenticate or authorize callers; keep the Service on a
trusted network and terminate authenticated TLS at an ingress or use
mesh mTLS before exposing it outside the cluster. A harness's per-job
bearer token authenticates its claim to that job, but does not provide
transport encryption or caller identity for the API/UI.

Harness streams are not trusted on job ID alone: submission mints a
per-job bearer token, launchers pass
`<job id>.<token>` as `CONTROL_PLANE_SESSION_ID`, and the control plane
rejects a `ready` whose token does not match (constant-time). Job IDs
are time-ordered ULIDs visible in pod names and URLs; without the token
a neighbouring pod could claim another job's stream and read its
RunConfig. Both connect services cap received messages at 4 MiB,
permission requests are capped per job, the web UI enforces same-origin
on state-changing requests, and graceful shutdown cancels live runs
before draining the listener and then waits for in-flight memory calls.

Memory widens the surface in two ways. Every run behind one hairpin
reads and writes one Billet namespace, so model-authored content saved
by one run reaches the context of later runs on any profile; run one
deployment per trust domain. And because `SubmitJob` accepts a
`run_config_json` that declares the memory tools, any caller who can
reach hairpin's port can read and write that namespace — the
NetworkPolicy in front of Billet restricts the network path, not the
API path. Hairpin dials Billet over plaintext h2c, so memory content
crosses the cluster network unencrypted alongside the control-plane
stream. The control plane answers a `tool_result_request` only for a
tool the job's stored RunConfig declared, caps concurrent and total
memory calls per run, and refuses repeated request IDs. See
[`docs/memory.md`](memory.md#trust-posture).

## Current limitations

- Follow-up turns (`followUpGrace` / `user_response`) are not supported;
  one hairpin job represents one run.
- `sandbox_token_request` is answered with a signed sandbox identity
  token only when `-sandbox-token-key` is configured (see
  [`docs/deployment.md`](deployment.md#sandbox-identity-tokens));
  otherwise it receives an explicit `is_error` refusal.
- Asynchronous tool results are answered only for the two memory
  tools; `SubmitJob` rejects a `tools.controlPlane` entry naming
  anything else. Batch requests are recorded but not answered, and a
  RunConfig that enables batch execution does not fail validation at
  submit time; see
  [issue #1](https://github.com/rxbynerd/hairpin/issues/1).
- Memory depends on two unmerged upstream changes:
  [stirrup PR #586](https://github.com/rxbynerd/stirrup/pull/586) for
  the `tools.controlPlane` RunConfig surface and
  [billet PR #1](https://github.com/rxbynerd/billet/pull/1) for a
  published Billet image. See [`docs/memory.md`](memory.md).
- Only one hairpin replica is safe because the live session registry is
  in-process; see [issue #4](https://github.com/rxbynerd/hairpin/issues/4).
- API/UI authentication, authorization, and transport TLS are not built
  in; see [issue #5](https://github.com/rxbynerd/hairpin/issues/5).
