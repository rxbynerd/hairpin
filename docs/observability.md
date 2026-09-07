# Observability

Hairpin emits structured logs on stderr always, and OpenTelemetry
traces and metrics when an exporter is configured. Export is off by
default: a development server needs no collector and pays no
instrumentation cost, because with no exporter selected the SDK is
never installed.

Two things are instrumented: the RPC surfaces (the `JobService` API and
the stirrup harness control plane, via
[`otelconnect`](https://github.com/connectrpc/otelconnect-go)), and
hairpin's own job lifecycle — submission, launch, harness stream,
outcome, latency, and the Billet-backed memory calls.

A run's *own* traces are a separate signal: stirrup takes a trace
emitter in its RunConfig and exports the agent loop from inside the
harness. Hairpin can name the collector for it (see [Run
traces](#run-traces)) but records none of it and does not propagate its
trace context into the harness process.

## Enabling export

| Flag | Default | Meaning |
|---|---|---|
| `-telemetry` | `none` | Exporter: `none`, `otlp`, or `stdout`. |
| `-telemetry-protocol` | `$OTEL_EXPORTER_OTLP_PROTOCOL`, else `grpc` | OTLP transport: `grpc` (port 4317) or `http/protobuf` (port 4318). |
| `-telemetry-endpoint` | *(empty)* | OTLP endpoint URL, overriding `OTEL_EXPORTER_OTLP_ENDPOINT`. An `http://` scheme selects cleartext. |
| `-telemetry-sample-ratio` | `1` | Head-sampling probability, 0 to 1, for traces hairpin starts. |
| `-telemetry-service-name` | *(empty)* | `service.name`, overriding the `hairpin` default but not `OTEL_SERVICE_NAME`. |
| `-telemetry-metric-interval` | `60s` | How often metrics are exported. |

These configure hairpin's export of its own signals only. The
collector a *run* exports to is named separately, by
`-harness-telemetry-endpoint` — see [Run traces](#run-traces).

Everything else is left to the environment variables the OpenTelemetry
SDK already reads — `OTEL_EXPORTER_OTLP_ENDPOINT`,
`OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`,
`OTEL_EXPORTER_OTLP_CERTIFICATE`, and their metric equivalents.

Against a collector in the same cluster:

```yaml
args:
  - serve
  - --telemetry=otlp
env:
  - name: OTEL_EXPORTER_OTLP_ENDPOINT
    value: http://otel-collector.observability.svc:4317
  - name: POD_NAME
    valueFrom:
      fieldRef:
        fieldPath: metadata.name
```

Sampling is parent-based: a caller who arrives with a sampled
`traceparent` is always followed, and the ratio applies only to traces
hairpin starts itself. A busy deployment can lower the ratio without
losing traces its callers already chose to sample.

## Run traces

Two endpoints are involved in a deployment that exports everything, and
they are not interchangeable:

| Flag | Exported by | Resolved from | Carries |
|---|---|---|---|
| `-telemetry-endpoint` | Hairpin's own SDK | Hairpin's Pod | The spans and metrics on this page. |
| `-harness-telemetry-endpoint` | The stirrup harness | The harness Pod | One run's `run`/`turn`/`tool_call` span tree and `stirrup.harness.*` metrics. |

`-harness-telemetry-endpoint` configures nothing in hairpin's own
process: it is an OTLP/gRPC `host:port` written into each submitted
RunConfig's `trace_emitter` as `{type: "otel", endpoint: <the flag>}`,
in the same way the [`-sandbox-*`
flags](deployment.md#sandbox-coordinates) fill in executor
coordinates, and only where the profile left `trace_emitter` empty. A
profile that names its own emitter — a `jsonl` file, a different
collector, a managed gateway with credentials — keeps it. The protocol
is left unset, so the harness applies its own default of gRPC.

The split matters because the two endpoints are reached from different
Pods and can differ: hairpin may export to a cluster-wide collector
while runs export to one that keeps their traces separate. Pointing
both at the same collector is fine, and is what
[`examples/k8s/`](../examples/k8s/) does.

Hairpin does not aggregate, read, or forward run traces. What a run
emits, and the attributes on it, are stirrup's contract.

## Resource attributes

Every span and metric carries the process's resource:

| Attribute | Source |
|---|---|
| `service.name` | `-telemetry-service-name`, else `OTEL_SERVICE_NAME`, else `hairpin`. |
| `service.version` | The module version stamped into the binary; `dev` for an unstamped build. |
| `service.instance.id` | `$POD_NAME`, else the hostname. |
| `host.name`, `process.pid`, `telemetry.sdk.*` | Detected. |

`OTEL_RESOURCE_ATTRIBUTES` and `OTEL_SERVICE_NAME` are applied last and
win over hairpin's defaults, so deployment-level attributes
(`deployment.environment.name`, a cluster name, a team) need no flag.
Setting `POD_NAME` from the downward API is worth doing: without it
`service.instance.id` is the Pod's hostname, which is the same string
but only by convention.

## Traces

| Span | Started by | Covers |
|---|---|---|
| `hairpin.v1.JobService/<Method>` | The RPC interceptor | One API call. |
| `stirrup.harness.v1.HarnessService/RunTask` | The RPC interceptor | One harness connection, for its whole life. |
| `hairpin.submit` | `internal/service` | Resolving, validating, and persisting one submission. |
| `hairpin.launch` | `internal/service` | Handing one harness to its launcher. |
| `hairpin.harness_session` | `internal/controlplane` | Task assignment through terminal status. |
| `hairpin.memory.call` | `internal/controlplane` | One `search_memory` or `save_memory` call to Billet. |

One run produces three traces, not one, and they are joined by links
rather than by parentage:

- The **submission** trace belongs to the caller. `hairpin.submit` is
  a child of the `SubmitJob` RPC span, and of the caller's own span if
  it propagated one.
- The **launch** trace stands alone. Launching detaches from the
  submitting request so a caller disconnect cannot orphan a harness,
  and a span outliving the trace it reported into would misrepresent
  that. `hairpin.launch` links back to the submission instead.
- The **harness stream** trace belongs to the harness, which dials in
  minutes later as a separate caller. `hairpin.harness_session` links
  back to the submission that created its job.

Those links survive a restart: the submission's `traceparent` is stored
on the job record, so the stream is linked even when the process that
accepted the submission is gone.

Span attributes carry the identifiers metrics deliberately omit:
`hairpin.job.id` on every hairpin span, `hairpin.profile` on a
submission that resolved one, and `hairpin.request.id` plus
`hairpin.memory.tool` on a memory call.

Per-message span events are disabled. A harness stream carries
thousands of `text_delta` events and one span event each would swamp
the trace; the `hairpin.harness.events` counter covers the volume
instead.

## Metrics

Hairpin's own instruments, alongside otelconnect's `rpc.server.*`
metrics for both RPC surfaces:

| Metric | Instrument | Unit | Attributes |
|---|---|---|---|
| `hairpin.job.submissions` | counter | `{submission}` | `hairpin.submission.outcome`: `accepted`, `rejected`, `failed`; `hairpin.profile` when one resolved. |
| `hairpin.job.launches` | counter | `{launch}` | `hairpin.launch.outcome`: `succeeded`, `failed`, `skipped`. |
| `hairpin.job.launch.duration` | histogram | `s` | `hairpin.launch.outcome`. |
| `hairpin.job.completions` | counter | `{job}` | `hairpin.job.status`, `hairpin.job.stop_reason`. |
| `hairpin.job.run.duration` | histogram | `s` | `hairpin.job.status`. |
| `hairpin.harness.sessions` | counter | `{session}` | `hairpin.session.disposition`. |
| `hairpin.harness.sessions.active` | up/down counter | `{session}` | — |
| `hairpin.harness.events` | counter | `{event}` | `hairpin.harness.event.type`. |
| `hairpin.permission.requests` | counter | `{request}` | — |
| `hairpin.permission.decisions` | counter | `{decision}` | `hairpin.permission.decision`: `allow` or `deny`; `hairpin.permission.delivered`. |
| `hairpin.memory.calls` | counter | `{call}` | `hairpin.memory.tool`, `hairpin.memory.outcome`: `ok`, `error`, `refused`. |
| `hairpin.memory.call.duration` | histogram | `s` | `hairpin.memory.tool`, `hairpin.memory.outcome`. |

Notes on reading them:

- `hairpin.job.completions` counts every terminal transition, including
  jobs that failed to launch and jobs cancelled before any harness
  connected. `hairpin.job.run.duration` measures assignment to terminal
  status, so it covers only jobs that actually ran.
- `hairpin.session.disposition` is the outcome of one inbound
  `RunTask` stream: `assigned`, or one of `closed_before_ready`,
  `not_ready`, `no_session_id`, `unknown_job`, `bad_token`,
  `closed_job`, `duplicate`, `unusable_run_config`,
  `assignment_failed`. A non-zero rate of `bad_token` or `unknown_job`
  is a workload dialling a control plane it has no job on.
- `hairpin.permission.delivered` distinguishes a decision that reached
  the harness from one recorded after the stream had gone.
- A refused memory call is counted but not timed: it never reached
  Billet.

### Cardinality

Job IDs, request IDs, harness event types, stop reasons, and tool names
are supplied by callers or by the harness, so none of them label a
metric as sent:

- Identifiers (`hairpin.job.id`, `hairpin.request.id`) appear on spans
  only.
- Stop reasons, event types, and tool names are matched against the
  known protocol vocabulary; anything else is counted as `other`. A new
  stirrup event type therefore lands in `other` until it is added to
  `internal/telemetry`.
- A rejected submission records no `hairpin.profile`: the profile name
  was whatever the caller typed. Accepted submissions record it, since
  the value came from the operator's profiles directory.

## Local development

`-telemetry=stdout` writes spans and metrics to stderr, which is enough
to see the instrumentation without running a collector:

```sh
./hairpin serve -launcher=none -advertise=127.0.0.1:8130 \
  -telemetry=stdout -telemetry-metric-interval=5s
```

## Not instrumented

- **Run traces.** They go from the harness straight to the collector
  `-harness-telemetry-endpoint` names; hairpin neither sees them nor
  links them to its own spans.
- **The web UI.** Its handlers produce no server spans, so a submit
  from the UI starts its own trace at `hairpin.submit` rather than
  under an HTTP span.
- **Redis and Kubernetes client calls.** Store and launcher latency is
  visible only as the enclosing span's duration.
- **Logs.** They stay on stderr; nothing is exported over OTLP.
