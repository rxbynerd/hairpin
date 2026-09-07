# JobService API reference

`hairpin.v1.JobService` is hairpin's caller-facing API, defined in
[`proto/hairpin/v1/hairpin.proto`](../proto/hairpin/v1/hairpin.proto)
and served via [connect-go](https://connectrpc.com/) on the same h2c
port as the web UI and the stirrup control plane
(`cmd/hairpin/serve.go`). Every RPC is reachable as gRPC, gRPC-Web, or
Connect's JSON/HTTP protocol; the examples below use the JSON form
since it needs nothing but curl.

```
curl -s http://<listen-addr>/hairpin.v1.JobService/<Method> \
  -H 'Content-Type: application/json' \
  -d '<request JSON>'
```

Field names in JSON are lowerCamelCase (protobuf-JSON convention);
request bodies below also accept snake_case for convenience since
that is what the proto source uses, but responses are always
lowerCamelCase.

## SubmitJob

Accepts a task and returns the job ID every other RPC keys on. The job
is persisted before the call returns; launch failures after that
surface as a status transition to `failed`, not as an RPC error.

Request fields (`SubmitJobRequest`):

| Field | Meaning |
|---|---|
| `prompt` | The task prompt. A non-empty value replaces the prompt in the selected profile or `run_config_json`; when empty, the config must already carry a prompt. |
| `profile` | Named RunConfig profile to resolve against. Empty selects the server's default profile. Mutually exclusive with `run_config_json`. |
| `run_config_json` | A complete stirrup RunConfig in protobuf-JSON form. Hairpin forces `run_id` to the job ID, applies a non-empty request `prompt`, and fills unset sandbox coordinates on `k8s`/`k8s-sandbox` executors from the server's `-sandbox-*` flags. See [Profiles](../README.md#profiles) for what "no CLI defaulting" means here — `mode`, `provider.type` (or a `providers` map), `executor.type`, `max_turns`, and `timeout` must all be explicit, or `SubmitJob` rejects the request with `invalid_argument`. |

Whichever source the RunConfig comes from, `SubmitJob` also rejects it
with `invalid_argument` when `tools.controlPlane` names a tool other
than `search_memory` or `save_memory`, names either while the server
runs without `-billet-addr`, or sets a non-zero `timeoutSeconds` below
hairpin's 10 second memory call timeout. See
[`docs/memory.md`](memory.md#submit-time-checks).

```sh
curl -s http://localhost:8130/hairpin.v1.JobService/SubmitJob \
  -H 'Content-Type: application/json' \
  -d '{"prompt": "summarize the open issues", "profile": "default"}'
```

```json
{"job": {"id": "hp-01j...", "status": "JOB_STATUS_QUEUED", "prompt": "summarize the open issues", "profile": "default", "createdAt": "2026-08-29T09:00:00Z"}, "harnessSession": "hp-01j....9b4c68c82fefa7e49c35b0f2cd602a85"}
```

`harness_session` (`SubmitJobResponse` only — no other RPC returns it)
is the bearer credential a harness must present, as
`CONTROL_PLANE_SESSION_ID` in the form `<job id>.<token>`, echoed back
in its `ready` event's `id` field, to claim this job's stream; a bare
job ID is rejected and the harness is sent `cancel`. The Kubernetes
launcher sets `CONTROL_PLANE_SESSION_ID` to this value automatically — it matters to a caller only when starting a harness
out-of-band with `-launcher none`. Treat it as a secret: it appears
exactly once, in this response, and is never included in `GetJob` /
`ListJobs` output.

## GetJob

Returns the current record for one job.

```sh
curl -s http://localhost:8130/hairpin.v1.JobService/GetJob \
  -H 'Content-Type: application/json' \
  -d '{"id": "hp-01j..."}'
```

```json
{"job": {"id": "hp-01j...", "status": "JOB_STATUS_RUNNING", "prompt": "summarize the open issues", "profile": "default", "createdAt": "...", "startedAt": "...", "lastEventAt": "..."}}
```

## ListJobs

Returns jobs newest-first.

| Field | Meaning |
|---|---|
| `limit` | Maximum jobs to return; the server defaults to 50 and caps at 100. |
| `page_token` | Opaque cursor from a previous response's `next_page_token`. |

```sh
curl -s http://localhost:8130/hairpin.v1.JobService/ListJobs \
  -H 'Content-Type: application/json' \
  -d '{"limit": 20}'
```

```json
{"jobs": [{"id": "hp-01j...", "status": "JOB_STATUS_SUCCEEDED", ...}, ...], "nextPageToken": ""}
```

`nextPageToken` is empty once there are no further pages.

## WatchJob

Streams a job's recorded events from a resume position, then follows
the live stream until the job reaches a terminal status or the caller
disconnects. Server streaming, so it needs a client that reads a
chunked/streamed response — curl works for a quick look but will
block until the job finishes or the connection is cut.

| Field | Meaning |
|---|---|
| `id` | Job ID. |
| `after_id` | Resume after this event ID; empty streams from the beginning. |

```sh
curl -s --no-buffer http://localhost:8130/hairpin.v1.JobService/WatchJob \
  -H 'Content-Type: application/json' \
  -d '{"id": "hp-01j..."}'
```

Each response frame carries one `JobEvent`:

```json
{"event": {"id": "1735500000000-0", "type": "text_delta", "payloadJson": "{\"type\":\"text_delta\",\"text\":\"...\"}", "at": "2026-08-29T09:00:01Z"}}
```

`event.id` is an opaque, store-assigned, increasing position (currently
Redis-stream-shaped) — pass it back as `after_id` on a reconnect to
resume without re-reading history already seen. This is also exactly
what the web UI's SSE endpoint does with the `Last-Event-ID` header
(see [WatchJob resume semantics](#watchjob-resume-semantics) below).

For a job that is already terminal, `WatchJob` replays its full
recorded history and then closes — there is nothing live left to
follow.

## CancelJob

Requests cancellation. A running harness is sent a `cancel`
`ControlEvent` and winds down within one turn boundary (git
finalisation still runs; the harness reports `done` with
`stop_reason: "cancelled"`). A job that has not been assigned yet
(no live harness session) is cancelled immediately. Cancelling an
already-terminal job is a no-op that returns the job as-is.

```sh
curl -s http://localhost:8130/hairpin.v1.JobService/CancelJob \
  -H 'Content-Type: application/json' \
  -d '{"id": "hp-01j..."}'
```

```json
{"job": {"id": "hp-01j...", "status": "JOB_STATUS_RUNNING", ...}}
```

The response status reflects the state at the moment of the call — a
running harness has not necessarily stopped yet; poll `GetJob` or
watch for the terminal `status_change` event to know when it has.

## ListPermissionRequests

Returns the permission requests recorded for a job, pending ones
first.

```sh
curl -s http://localhost:8130/hairpin.v1.JobService/ListPermissionRequests \
  -H 'Content-Type: application/json' \
  -d '{"job_id": "hp-01j..."}'
```

```json
{"permissionRequests": [{"requestId": "req-1", "toolName": "run_command", "inputJson": "{\"command\":\"rm -rf /tmp/x\"}", "state": "pending", "requestedAt": "2026-08-29T09:00:05Z"}]}
```

## AnswerPermission

Resolves a pending `permission_request` from the harness.

| Field | Meaning |
|---|---|
| `job_id` | Job ID. |
| `request_id` | The `request_id` from the `permission_request` event. |
| `allow` | `true` to allow the tool call, `false` to deny. |
| `reason` | Optional context passed back to the model on denial. |

```sh
curl -s http://localhost:8130/hairpin.v1.JobService/AnswerPermission \
  -H 'Content-Type: application/json' \
  -d '{"job_id": "hp-01j...", "request_id": "req-1", "allow": false, "reason": "not in this environment"}'
```

```json
{"permissionRequest": {"requestId": "req-1", "toolName": "run_command", "inputJson": "...", "state": "denied", "reason": "not in this environment", "requestedAt": "...", "answeredAt": "..."}}
```

Answering a request that Hairpin has already marked allowed or denied
returns `invalid_argument`. Answering when the job has no live harness
session returns `failed_precondition`.

The harness auto-denies a request when its policy timeout expires, but
it does not send Hairpin an expiration event. The stored request can
therefore remain `pending`; a late answer may be accepted by this API
even though the harness ignores it. Callers should answer within the
configured timeout. Tracking expiration accurately is
[issue #2](https://github.com/rxbynerd/hairpin/issues/2).

## Errors

Every RPC maps internal errors onto connect codes (`internal/api/api.go`):

| Condition | Code |
|---|---|
| Job, event, or permission request not found | `not_found` |
| Malformed job ID or RunConfig, unknown profile, missing prompt, unsupported control-plane tool declaration, non-pending permission answer | `invalid_argument` |
| Operation needed a live harness stream and there was none | `failed_precondition` |
| Anything else | `internal` |

## Job lifecycle

| Status | Meaning |
|---|---|
| `JOB_STATUS_QUEUED` | Accepted and persisted; launcher not yet invoked. |
| `JOB_STATUS_LAUNCHING` | Launcher invoked; the harness Job is being created. |
| `JOB_STATUS_AWAITING_HARNESS` | Harness started but has not yet dialled back in with `ready`. |
| `JOB_STATUS_RUNNING` | `task_assignment` sent; the harness is executing. |
| `JOB_STATUS_SUCCEEDED` | Terminal. `done.stop_reason` was `"success"`. |
| `JOB_STATUS_FAILED` | Terminal. Any non-success, non-cancelled outcome — including launch errors and a stream that closed without a `done` event. See `Job.stop_reason` and `Job.error`. |
| `JOB_STATUS_CANCELLED` | Terminal. Cancelled via `CancelJob` (`stop_reason: "cancelled"`, or cancelled before assignment). |

`stop_reason` carries stirrup's `done.stop_reason` verbatim. For
`stirrup job` this is the run *outcome*: `success`, `error`,
`max_turns`, `verification_failed`, `verification_error`,
`budget_exceeded`, `stalled`, `tool_failures`, `cancelled`, `timeout`,
`max_tokens`, plus feature-specific values such as `setup_failed`,
`hook_failed`, `guardrail_blocked`, and `rule_of_two_violation`.
Unknown values are preserved rather than mapped away — stirrup adds
outcomes over time, and anything that is not `success` or `cancelled`
maps to `JOB_STATUS_FAILED`.

## Event types

`WatchJob` and the web UI's SSE feed emit one `JobEvent` per recorded
entry on a job's timeline. `type` is the originating
`stirrup.harness.v1.HarnessEvent` type discriminator, plus one type
hairpin synthesises itself:

| `type` | Origin | Notes |
|---|---|---|
| `text_delta` | harness | Incremental model output text. Hairpin coalesces consecutive deltas before persisting them, so the recorded timeline has fewer, larger fragments than the raw harness stream. |
| `tool_call` | harness | `id`, `name`, `input`. In `payloadJson`, protobuf encodes the `bytes` input as base64; decoding it yields the JSON tool arguments. |
| `tool_result` | harness | `tool_use_id`, `content`. |
| `permission_request` | harness | `request_id`, `tool_name`, base64-encoded `input`. Also recorded in the permissions store with decoded JSON as `inputJson` — see [`ListPermissionRequests`](#listpermissionrequests). |
| `heartbeat` | harness | No payload; liveness only. Sent every 30s during execution. |
| `warning` | harness | `message`; non-fatal. |
| `error` | harness | `message`. The normal failure path follows it with `done` carrying `stop_reason: "error"`; transport loss can still end the stream first. |
| `done` | harness | `stop_reason`, and `trace` when the harness populated it. Always the last harness-originated event of a run. |
| `sandbox_token_request` | harness | Recorded, then hairpin immediately answers with an explicit refusal — see [Unsupported protocol capabilities](#unsupported-protocol-capabilities). |
| `tool_result_request` | harness | `request_id`, `tool_use_id`, `tool_name`, base64-encoded `input`. A call to a control-plane tool. Hairpin answers `search_memory` and `save_memory` calls the run declared by proxying them to Billet, and refuses anything else — see [`docs/memory.md`](memory.md). |
| `tool_result_response` | hairpin | `request_id`, `content`, `is_error`. The `ControlEvent` hairpin sent in answer to a `tool_result_request`, recorded whether or not delivery to the harness succeeded. |
| `batch_submission` | harness | Recorded but not answered — see [Unsupported protocol capabilities](#unsupported-protocol-capabilities). |
| `batch_waiting`, `batch_cancel_request` | harness | Recorded without batch-provider action; batch execution is unsupported. |
| `status_change` | hairpin | Synthesised whenever hairpin moves a job between statuses. Payload: `{"status": "<job status>", "error": "<optional>"}`. This is what a watcher uses to detect job completion — see [WatchJob resume semantics](#watchjob-resume-semantics). |

Unknown harness event types are recorded verbatim rather than dropped,
so a timeline never silently loses events stirrup adds in the future.

## Permission flow

A RunConfig with `permission_policy.type: "ask-upstream"` routes
approval-required tool calls to the control plane. Tools that do not
require approval are allowed harness-side without this round trip:

1. The harness wants to run a tool and sends a `permission_request`
   `HarnessEvent` (`request_id`, `tool_name`, `input`) on the open
   `RunTask` stream.
2. Hairpin's control plane (`internal/controlplane/pump.go`) persists
   it as a pending `PermissionRequest` and appends the raw event to
   the job's timeline. It does not decide anything itself.
3. A caller — the JobService API, or the approve/deny buttons on the
   web UI's job detail page — sees the pending request via
   `ListPermissionRequests` or the live `WatchJob`/SSE feed, and calls
   `AnswerPermission`.
4. `AnswerPermission` records the decision, then sends a
   `permission_response` `ControlEvent` (`request_id`, `allowed`,
   `reason`) onto the harness's live stream through the in-process
   session registry. If delivery fails, Hairpin restores the pending
   record so the caller can retry. A denial's `reason` is passed to the
   model as context.

If nobody answers, the harness denies the call when its policy timeout
(default 60s) expires. Hairpin does not enforce or observe that timeout,
so its stored state may remain `pending`; see the warning under
[`AnswerPermission`](#answerpermission).

## WatchJob resume semantics

Both `WatchJob` and the web UI's `/jobs/{id}/events` SSE endpoint
serve the same underlying event source
(`internal/service.Service.Watch`) and share resume semantics:

- **Position** is an opaque, increasing event ID
  (`internal/store.Event.ID`, currently Redis-stream-shaped). There is
  no separate cursor format — callers resume from the ID they already
  received.
- **`after_id` / `Last-Event-ID`**: `WatchJob.after_id` and the SSE
  `Last-Event-ID` request header (which browsers set automatically on
  reconnect) mean the same thing — resume strictly after this
  position. A plain `?after=<id>` query parameter works too, for
  clients that cannot set custom headers.
- **History then live**: a non-terminal job replays recorded history
  from the resume position first, then switches to live delivery of
  new events as the harness produces them.
- **Terminal jobs**: once a job has reached a terminal status, both
  endpoints only ever replay recorded history — there is nothing live
  left, so the stream (or SSE connection, after an `event: eof`
  frame) closes once history is exhausted.
- **Completion signal**: the channel/connection closes once it has
  delivered the `status_change` event announcing a terminal status (or
  immediately, for a job that was already terminal when watched, once
  history runs out). A caller does not need a separate "is this job
  done" check — event exhaustion on this endpoint means "no further
  activity is coming for the position you asked for."

## Unsupported protocol capabilities

Hairpin currently supports one harness run per job and does not
implement these optional parts of the stirrup protocol:

- Follow-up turns (`followUpGrace` / `user_response`).
- Sandbox identity token issuance. A `sandbox_token_request` receives an
  explicit `is_error` refusal so the harness fails before creating a
  sandbox.
- Batch execution. Related events are recorded, but Hairpin does not
  send the required provider result.
- Asynchronous tool results for any control-plane tool other than the
  two memory tools. `SubmitJob` rejects a `tools.controlPlane` entry
  with another name (see [SubmitJob](#submitjob)), and a
  `tool_result_request` for an undeclared or unknown tool receives an
  immediate `is_error` refusal.

`SubmitJob` does not yet reject a RunConfig that enables batch
execution or follow-up turns. Do not enable them through Hairpin;
fail-fast capability validation is tracked in
[issue #1](https://github.com/rxbynerd/hairpin/issues/1). Deployment-wide
limitations such as single-replica operation and missing API auth are
listed in [`docs/design.md`](design.md#current-limitations).
