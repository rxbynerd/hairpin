# hairpin

Hairpin provides a service control plane for
[stirrup](https://github.com/rxbynerd/stirrup). `stirrup job` is a
client: it dials the gRPC address in `CONTROL_PLANE_ADDR` and asks for
work. Hairpin owns that long-lived bidirectional stream so callers such
as webhook handlers can submit work through a request/response API
instead of hosting a gRPC server for the lifetime of a run.

Hairpin is that control plane, packaged as a service. A caller submits
a task over a plain request/response API and gets back a job ID.
Hairpin launches a `stirrup job` on its cluster, hands the task across
when the harness dials back in, records everything the harness
streams, and lets the original caller — or anyone else, such as the
bundled web UI — retrieve status, events, and the result by job ID.

The name follows the equestrianism series (stirrup, haybale, billet):
a hairpin is the turn that reverses the direction of travel.

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

`SubmitJob` returns as soon as the job is durable; hairpin then drives
it through `queued` → `launching` → `awaiting_harness` → `running` →
a terminal status in the background. Poll `GetJob`, stream `WatchJob`,
or open the web UI to follow along. See
[`docs/design.md`](docs/design.md) for the full lifecycle and
component breakdown.

### Shared memory

With `-billet-addr` set, a profile can declare two control-plane tools,
`search_memory` and `save_memory`, backed by
[Billet](https://github.com/rxbynerd/billet). The harness raises each
call up the `RunTask` stream as a `tool_result_request`; hairpin
proxies it to Billet over gRPC and answers with a
`tool_result_response`. The sandbox gains no network path to Billet,
and knowledge one run saves is searchable by the next. Both events are
recorded on the job timeline. See [`docs/memory.md`](docs/memory.md)
for the tool contracts, the limits hairpin enforces, and the trust
posture of a namespace shared by every run.

## Quick start

Hairpin is a Kubernetes application: it runs inside the cluster whose
API it uses, launches each run as a `batch/v1` Job from the published
stirrup harness image, and that harness in turn creates one sandbox Pod
per run from the stirrup sandbox image.

A development cluster, from nothing to a completed run:

```sh
just kind-up             # a single-node kind cluster on podman
just deploy              # build hairpin, load it, apply examples/k8s + a fake provider
just smoke-test          # submit one job and assert it ran in a sandbox Pod
just memory-smoke-test   # submit two jobs and assert the second recalls what the first saved
just kind-down
```

`just deploy` also installs a development-only stand-in model provider
(`scripts/dev/fake-provider.yaml`) so a run completes without an API
key or any egress from the cluster. To run against a real model, `just
openrouter <op-ref>` adds an OpenRouter-backed `openrouter` profile,
reading the API key from 1Password; submit with
`{"profile": "openrouter"}`. Re-run it after each `just deploy`.

The fake provider's run calls the memory tools, so both smoke tests
depend on two upstream pieces that are not yet published: `just
deploy` builds Billet from a sibling checkout at `BILLET_DIR` (default
`../billet`) because `ghcr.io/rxbynerd/billet:latest` does not exist
until [billet PR #1](https://github.com/rxbynerd/billet/pull/1)
merges, and the harness must be built from
[stirrup PR #586](https://github.com/rxbynerd/stirrup/pull/586) rather
than the published `stirrup:latest`. See
[`docs/memory.md`](docs/memory.md#the-development-cluster).

For a real cluster, apply [`examples/k8s/`](examples/k8s/) and supply
your own provider Secret. An in-cluster hairpin needs almost no
configuration: it reads its namespace from its projected
ServiceAccount, advertises `hairpin.<namespace>.svc` on its listen
port, and defaults both images to the published tags.

```sh
./hairpin serve \
  --redis redis.hairpin.svc:6379 \
  --profiles /etc/hairpin/profiles \
  --harness-service-account stirrup-harness \
  --sandbox-namespace hairpin-sandboxes
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
enough. The Kubernetes launcher injects it automatically; it only
matters to a caller when starting a harness out-of-band with
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
given), and hairpin applies these server-owned values before launch:

| Field | Filled with |
|---|---|
| `run_id` | The hairpin job ID, forced unconditionally — a caller cannot override it. |
| `prompt` | A non-empty request `prompt`; otherwise the template's prompt is retained. |
| `executor.image` | `-sandbox-image`, when the template names none. |
| `executor.k8sNamespace` | `-sandbox-namespace`, when the template names none. |
| `executor.k8sServiceAccount` | `-sandbox-service-account`, when the template names none. |
| `executor.runtime` | `-sandbox-runtime`, when the template names none. |
| `trace_emitter` | `{type: "otel", endpoint: -harness-telemetry-endpoint}`, when the template names none. |

The four executor fields are filled only for the `k8s` and
`k8s-sandbox` executors, and only where the template left them empty —
a profile that pins its own sandbox image or namespace keeps it. This
lets a profile describe its isolation requirements while deployment
configuration supplies the default cluster location and identity.
`trace_emitter` follows the same rule, so a profile stays portable
across clusters instead of naming a collector; one that sets its own
emitter — a file, a different collector — keeps it.

All other template values are preserved. Unlike `stirrup
harness`'s CLI, there is no defaulting on the wire: `mode`,
`provider.type` (or a `providers` map), `executor.type`, `max_turns`,
and `timeout` must all be explicit in the profile or in an explicit
`run_config_json` — `internal/service/submit.go` rejects a submission
that omits any of them before it ever reaches a launcher.
`executor.type` in particular has no safe implicit default: an
omitted executor defaults to `"local"` harness-side, running the
agent's shell commands directly in the harness process rather than a
sandbox, so hairpin requires it to be a deliberate choice. For the
Pod-backed executors hairpin also mirrors stirrup's cross-field rules
at submit — a missing `network.mode`, a `workspace` the Pod has no way
to mount, `allowlist` egress with no proxy URL — so a run that could
never construct its executor is refused before a Job is created rather
than after a Pod is scheduled. A caller
may skip profiles entirely and pass a complete `run_config_json`,
which is mutually exclusive with `profile`.

## Flag reference

Flags for `hairpin serve`, from `cmd/hairpin/serve.go`:

| Flag | Default | Meaning |
|---|---|---|
| `-listen` | `:8130` | h2c listen address serving the API, control plane, and web UI. |
| `-advertise` | `hairpin.<namespace>.svc:<port>` | Address harnesses dial back into (`CONTROL_PLANE_ADDR`). Defaulted only when the namespace is known; required otherwise. |
| `-redis` | *(empty)* | Redis `host:port`. Empty selects the in-memory store (dev only). |
| `-redis-password` | *(empty)* | Redis password. |
| `-redis-db` | `0` | Redis database number. |
| `-billet-addr` | *(empty)* | `host:port` of Billet's RPC listener, backing the `search_memory` and `save_memory` tools; both halves required, numeric port. Empty disables memory and rejects submits that declare the tools. See [Shared memory](#shared-memory). |
| `-launcher` | `kubernetes` | Harness launcher: `kubernetes` or `none`. |
| `-profiles` | *(empty)* | Directory of RunConfig profile templates (`<name>.json`). |
| `-default-profile` | `default` | Profile used when a submit names none. |
| `-retention` | `0` | Delete a terminal job's record, timeline, and permissions this long after it finishes. `0` keeps jobs forever. |
| `-namespace` | *(the Pod's own)* | Namespace harness Jobs are created in, read from the projected ServiceAccount when running in-cluster. |
| `-harness-image` | `ghcr.io/rxbynerd/stirrup:latest` | Harness image run as the Job. |
| `-harness-service-account` | *(empty)* | ServiceAccount for harness Pods. Its token is mounted so the sandbox executor can reach the API; empty mounts none. |
| `-harness-secrets` | *(empty)* | Comma-separated Secret names exposed to harness Pods via `envFrom`. |
| `-kubeconfig` | *(empty)* | Kubeconfig path. Empty prefers in-cluster config, then `$KUBECONFIG`. |
| `-job-ttl` | `3600` | `ttlSecondsAfterFinished` on created harness Jobs. |
| `-deadline-slack` | `10m` | Slack added to the RunConfig timeout for the Job's `activeDeadlineSeconds`. |
| `-sandbox-image` | `ghcr.io/rxbynerd/stirrup-sandbox:latest` | Image sandbox Pods run. Must ship a shell, `tar`, `ls`, and `mkdir`. |
| `-sandbox-namespace` | *(`-namespace`)* | Namespace sandbox Pods and their NetworkPolicies are created in. |
| `-sandbox-service-account` | *(empty)* | ServiceAccount for sandbox Pods. Its token is never mounted. |
| `-sandbox-runtime` | *(cluster default)* | `RuntimeClassName` for sandbox Pods: `runc`, `gvisor`, `kata-qemu`, `kata-fc`, `kata-clh`. |
| `-sandbox-token-key` | *(empty)* | Path to an ES256 (P-256) private key PEM for signing sandbox identity tokens. Empty disables issuance (`sandbox_token_request` is refused). |
| `-sandbox-token-issuer` | *(empty)* | `iss` claim on minted sandbox identity tokens. Required when `-sandbox-token-key` is set. |
| `-sandbox-token-audience` | *(empty)* | `aud` claim on minted sandbox identity tokens. Required when `-sandbox-token-key` is set; always wins over the harness's requested audience. |
| `-sandbox-token-ttl` | `15m` | TTL of minted sandbox identity tokens. |
| `-harness-telemetry-endpoint` | *(empty)* | OTLP/gRPC `host:port`, resolved from the harness Pod, injected as a submitted RunConfig's `trace_emitter` where it names none. The harness exports the run's own trace there. |
| `-telemetry` | `none` | OpenTelemetry exporter: `none`, `otlp`, or `stdout`. See [`docs/observability.md`](docs/observability.md). |
| `-telemetry-protocol` | `$OTEL_EXPORTER_OTLP_PROTOCOL`, else `grpc` | OTLP transport: `grpc` or `http/protobuf`. |
| `-telemetry-endpoint` | *(empty)* | OTLP endpoint URL, overriding `OTEL_EXPORTER_OTLP_ENDPOINT`. |
| `-telemetry-sample-ratio` | `1` | Head-sampling probability, 0 to 1, for traces hairpin starts. |
| `-telemetry-service-name` | *(empty)* | `service.name`, overriding the `hairpin` default but not `OTEL_SERVICE_NAME`. |
| `-telemetry-metric-interval` | `60s` | How often metrics are exported. |

The five `-telemetry-*` flags are inert unless `-telemetry` selects an
exporter; endpoint, headers, and resource attributes otherwise come
from the standard `OTEL_*` environment variables. They cover hairpin's
own signals only — `-harness-telemetry-endpoint` is independent of
them, and of each other's collectors.

The `-sandbox-image`, `-sandbox-namespace`,
`-sandbox-service-account`, and `-sandbox-runtime` flags, and
`-harness-telemetry-endpoint`, do not configure hairpin's own
behaviour — they are the values it writes into each submitted
RunConfig, into the executor and `trace_emitter` respectively. See
[Profiles](#profiles). The `-sandbox-token-*` flags configure hairpin
as a JWT issuer for haybale's git credential proxy — see [Sandbox
identity tokens](docs/deployment.md#sandbox-identity-tokens).

`hairpin` has one other subcommand: `hairpin keygen --out <path>
[--jwks-out <path>]` generates the ES256 signing keypair issuance
needs, writing the private key PEM `-sandbox-token-key` reads and the
matching JWKS document haybale verifies against, and printing the
key's `kid`. Neither file is overwritten if it already exists.

`-launcher=none` disables harness launching entirely: jobs sit in
`awaiting_harness` until something starts a harness out-of-band with
`CONTROL_PLANE_SESSION_ID` set to the `harnessSession` value returned by
`SubmitJob`. This mode is useful for tests and external launchers.

## Trust posture

Hairpin's single listener uses plaintext h2c. The JobService API and web
UI do not authenticate or authorize callers, so keep the Service on a
trusted network and put authentication plus TLS (or mesh mTLS) in front
of it before exposing it outside the cluster. Per-job harness tokens
authenticate a harness's claim to one job, but they do not encrypt the
stream or authenticate API/UI callers. See
[`docs/design.md`](docs/design.md#trust-posture) for details.

Within that trusted-network posture, hairpin includes these controls:

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
- **Hardened harness Pod `securityContext`**: seccomp
  `RuntimeDefault`, all capabilities dropped, no privilege escalation,
  and a writable `/tmp` provided via `emptyDir` rather than a broader
  writable root filesystem.
- **Graceful shutdown cancels live runs.** On SIGTERM/SIGINT, hairpin
  sends `cancel` to every live harness session before draining the
  listener, so a restart doesn't strand in-flight jobs stuck `running`
  with no terminal record. It then waits for in-flight memory calls so
  their answers are recorded.
- **Memory calls are admitted per run, not per request.** A
  `tool_result_request` is answered only for a tool the job's stored
  RunConfig declared, at most four at a time and 1000 per run, and a
  repeated `request_id` is refused. Memory itself is one namespace
  shared by every run behind a hairpin, and reaching hairpin's API is
  equivalent to reaching Billet — see
  [`docs/memory.md`](docs/memory.md#trust-posture) before mixing trust
  domains on one deployment.
- **Sandbox identity tokens are off by default.** With
  `-sandbox-token-key` configured, hairpin becomes a credential issuer:
  any caller who can reach `SubmitJob` can name a `repo_scope` and get
  a run holding a JWT haybale honours, bounded only by the `repo_scope`
  it asked for and haybale's own default-deny policy. Issuance is
  capped per run and refused for a run whose config declares no
  `executor.sandbox_identity`, but the submitter supplies that config,
  so neither is an authorization boundary. An unauthenticated API in
  front of a credential issuer is a sharper trade than one in front of
  compute alone — read
  [`docs/design.md`](docs/design.md#trust-posture) before enabling it.

## Documentation

| Topic | Doc |
|---|---|
| Architecture, job lifecycle, component responsibilities, Redis layout | [`docs/design.md`](docs/design.md) |
| `JobService` RPC reference, event types, permission flow, watch/resume semantics | [`docs/api.md`](docs/api.md) |
| Kubernetes deployment recipe, Redis guidance, operational notes | [`docs/deployment.md`](docs/deployment.md) |
| Shared memory: the Billet-backed tools, submit and run-time checks, limits, trust posture | [`docs/memory.md`](docs/memory.md) |
| OpenTelemetry export: configuration, spans, metrics, correlation, cardinality | [`docs/observability.md`](docs/observability.md) |
| Reference Kubernetes manifests | [`examples/k8s/`](examples/k8s/) |
| Current limitations and roadmap | [`TODO.md`](TODO.md) |

## License

Apache 2.0. See [`LICENSE`](LICENSE).
