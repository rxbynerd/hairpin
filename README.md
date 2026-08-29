# hairpin

Hairpin reverses the client-server direction of
[stirrup](https://github.com/rxbynerd/stirrup). `stirrup job` is
intentionally a *client*: it dials the gRPC address in
`CONTROL_PLANE_ADDR` and asks for work. That is the right shape for an
organisation running a full control plane, but it asks a lot of
event-driven callers — a Lambda handling a webhook should not have to
host a bidirectional gRPC server for the lifetime of a run just to use
stirrup.

Hairpin is that control plane, packaged as a service. A caller submits
a task over a plain request/response API and gets back a job ID.
Hairpin launches a `stirrup job` on its cluster, hands the task across
when the harness dials back in, records everything the harness
streams, and lets the original caller — or anyone else, such as the
bundled web UI — retrieve status, events, and the result by job ID.

The name follows the equestrianism series (stirrup, haybale): a
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
```

`SubmitJob` returns as soon as the job is durable; hairpin then drives
it through `queued` → `launching` → `awaiting_harness` → `running` →
a terminal status in the background. Poll `GetJob`, stream `WatchJob`,
or open the web UI to follow along. See
[`docs/design.md`](docs/design.md) for the full lifecycle and
component breakdown.

## Quick start

```sh
just build   # produces ./hairpin
```

Redis is optional for local development — omitting `-redis` uses an
in-memory store that is lost on restart. The `process` launcher runs
`stirrup job` as a local subprocess instead of creating a Kubernetes
Job, which is the fastest loop for trying hairpin out:

```sh
./hairpin serve \
  -listen :8130 \
  -advertise 127.0.0.1:8130 \
  -launcher process \
  -stirrup-bin /path/to/stirrup
```

Submit a job. Connect's JSON protocol speaks plain HTTP/1.1, so a curl
one-liner works against the same endpoint a gRPC client would dial —
no profile is configured here, so the request carries a complete
RunConfig as `run_config_json` (see [Profiles](#profiles) for the
alternative):

```sh
curl -s http://localhost:8130/hairpin.v1.JobService/SubmitJob \
  -H 'Content-Type: application/json' \
  -d '{
    "run_config_json": "{\"mode\":\"review\",\"prompt\":\"say hello\",\"provider\":{\"type\":\"anthropic\",\"apiKeyRef\":\"secret://ANTHROPIC_API_KEY\"},\"permissionPolicy\":{\"type\":\"ask-upstream\"},\"executor\":{\"type\":\"local\"},\"tools\":{\"builtIn\":[\"read_file\"]},\"maxTurns\":5,\"timeout\":300}"
  }'
# {"job":{"id":"hp-01m...", "status":"JOB_STATUS_QUEUED", "prompt":"say hello", ...}, "harnessSession":"hp-01m....<token>"}
```

`harnessSession` is the bearer credential a harness must present
(as `CONTROL_PLANE_SESSION_ID`, echoed back in the harness's `ready`
event) to claim this job's stream — a bare job ID is no longer
enough. The `process` and `k8s` launchers inject it automatically;
it only matters to a caller when starting a harness out-of-band with
`-launcher none`. It appears once, in this response — treat it as a
secret and see [`docs/api.md`](docs/api.md#submitjob) for details.

Poll it by ID:

```sh
curl -s http://localhost:8130/hairpin.v1.JobService/GetJob \
  -H 'Content-Type: application/json' \
  -d '{"id": "hp-01m..."}'
```

Or open `http://localhost:8130/` for the bundled web UI: a job list, a
submit form, and a per-job detail page with a live Server-Sent Events
feed and permission approve/deny buttons.

See [`docs/api.md`](docs/api.md) for the full RPC reference and
[`docs/deployment.md`](docs/deployment.md) for running on Kubernetes.

## JSON or gRPC — same endpoints

`JobService` and the vendored `stirrup.harness.v1.HarnessService` are
both served by [connect-go](https://connectrpc.com/) on one h2c port.
The same route handles gRPC, gRPC-Web, and Connect's JSON/HTTP
protocol, so `stirrup job`'s grpc-go client, any gRPC client, and
plain `curl -H 'Content-Type: application/json'` all work against the
same listener — pick whichever fits the caller. There is no separate
REST surface or OpenAPI shadow to keep in sync.

## Profiles

A **profile** is a named [protobuf-JSON](https://protobuf.dev/programming-guides/json/)
`RunConfig` template — one `<name>.json` file per profile in the
directory passed to `-profiles`. `SubmitJob` resolves a task against a
profile by name (or the server's `-default-profile` when none is
given), and hairpin fills in two fields before launch:

| Field | Filled with |
|---|---|
| `run_id` | The hairpin job ID, forced unconditionally — a caller cannot override it. |
| `prompt` | The request's `prompt`, but only when the profile carries none. |

Everything else in the template is used verbatim. Unlike `stirrup
harness`'s CLI, there is no defaulting on the wire: `mode`,
`provider.type` (or a `providers` map), `executor.type`, `max_turns`,
and `timeout` must all be explicit in the profile or in an explicit
`run_config_json` — `internal/service/submit.go` rejects a submission
that omits any of them before it ever reaches a launcher.
`executor.type` in particular has no safe implicit default: an
omitted executor defaults to `"local"` harness-side, running the
agent's shell commands directly in the harness process rather than a
sandbox, so hairpin requires it to be a deliberate choice. A caller
may skip profiles entirely and pass a complete `run_config_json`,
which is mutually exclusive with `profile`.

## Flag reference

Flags for `hairpin serve`, from `cmd/hairpin/serve.go`:

| Flag | Default | Meaning |
|---|---|---|
| `-listen` | `:8130` | h2c listen address serving the API, control plane, and web UI. |
| `-advertise` | *(required)* | Address harnesses dial back into (`CONTROL_PLANE_ADDR`). |
| `-redis` | *(empty)* | Redis `host:port`. Empty selects the in-memory store (dev only). |
| `-redis-password` | *(empty)* | Redis password. |
| `-redis-db` | `0` | Redis database number. |
| `-launcher` | `none` | Harness launcher: `k8s`, `process`, or `none`. |
| `-profiles` | *(empty)* | Directory of RunConfig profile templates (`<name>.json`). |
| `-default-profile` | `default` | Profile used when a submit names none. |
| `-stirrup-bin` | *(empty)* | Process launcher: path to the stirrup binary. |
| `-stirrup-workdir` | *(empty)* | Process launcher: harness working directory (empty: per-job temp dir). |
| `-stirrup-inherit-env` | `true` | Process launcher: pass hairpin's environment to the harness. |
| `-k8s-namespace` | *(empty)* | k8s launcher: namespace for harness Jobs. |
| `-k8s-image` | *(empty)* | k8s launcher: stirrup container image. |
| `-k8s-kubeconfig` | *(empty)* | k8s launcher: kubeconfig path (empty: in-cluster, then `$KUBECONFIG`). |
| `-k8s-service-account` | *(empty)* | k8s launcher: ServiceAccount for harness Pods. |
| `-k8s-env-from-secrets` | *(empty)* | k8s launcher: comma-separated Secret names exposed to harness Pods via `envFrom`. |
| `-k8s-job-ttl` | `3600` | k8s launcher: Job `ttlSecondsAfterFinished`. |
| `-k8s-deadline-slack` | `10m` | k8s launcher: slack added to the RunConfig timeout for the Job's `activeDeadlineSeconds`. |

`-launcher=none` disables harness launching entirely: jobs sit in
`awaiting_harness` until something starts a `stirrup job` out-of-band
with `CONTROL_PLANE_SESSION_ID` set to the job ID. Useful for tests and
for driving the harness lifecycle from outside hairpin.

## Trust posture

Hairpin inherits stirrup v0.1's plaintext, unauthenticated gRPC
posture: run it on a trusted network (cluster-internal Service, mesh
mTLS). The JobService API and web UI carry no authentication of their
own either — front them with your ingress's auth. Do not expose either
port publicly. See [`docs/design.md`](docs/design.md#trust-posture-v01)
for the full rationale and the features this posture deliberately
defers.

Within that unchanged plaintext/trusted-network posture, hairpin
hardens the specific risks a shared, unauthenticated control plane
creates:

- **Per-job harness session tokens.** `SubmitJob` mints a random
  128-bit token per job; a harness must present it (as part of
  `CONTROL_PLANE_SESSION_ID`) to claim that job's stream, and hairpin
  compares it in constant time. A bare, guessable job ID (a
  time-ordered ULID, visible in pod names and URLs) is no longer
  sufficient for another workload on the same network to hijack a
  job's stream. See [Quick start](#quick-start) above.
- **Bounded request sizes.** Both the JobService API and the harness
  control plane cap incoming message size at 4 MiB, and a job accepts
  at most 1000 recorded permission requests — a misbehaving or hostile
  peer can't exhaust memory with an oversized or unbounded stream.
- **Web UI CSRF hardening.** `X-Frame-Options: DENY`,
  `X-Content-Type-Options: nosniff`, and a `default-src 'self'`
  Content-Security-Policy on every response, plus a same-origin check
  on state-changing requests (submit, cancel, approve/deny). This
  matters specifically for deployments that front the UI with
  cookie-based SSO: without it, a cross-site form post would ride that
  cookie straight into a job action.
- **Hardened harness Pod `securityContext`** (k8s launcher): seccomp
  `RuntimeDefault`, all capabilities dropped, no privilege escalation,
  and a writable `/tmp` provided via `emptyDir` rather than a broader
  writable root filesystem.
- **Graceful shutdown cancels live runs.** On SIGTERM/SIGINT, hairpin
  sends `cancel` to every live harness session before draining the
  listener, so a restart doesn't strand in-flight jobs stuck `running`
  with no terminal record.

## Documentation

| Topic | Doc |
|---|---|
| Architecture, job lifecycle, component responsibilities, Redis layout | [`docs/design.md`](docs/design.md) |
| `JobService` RPC reference, event types, permission flow, watch/resume semantics | [`docs/api.md`](docs/api.md) |
| Kubernetes deployment recipe, Redis guidance, operational notes | [`docs/deployment.md`](docs/deployment.md) |
| Reference Kubernetes manifests | [`examples/k8s/`](examples/k8s/) |

## License

Apache 2.0. See [`LICENSE`](LICENSE).
