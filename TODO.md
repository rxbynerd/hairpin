# Roadmap

Hairpin's current implementation is Kubernetes-native, supports one run
per job, and requires a single replica. Future work
is tracked in GitHub rather than as a session log:

## Reliability and operations

- [Add job retention and stale-job reconciliation](https://github.com/rxbynerd/hairpin/issues/3): `--retention` deletes terminal jobs past the window and the same reaper fails jobs that never dial back. Still open for a job that loses its harness mid-run, which is reported through `last_event_at` but never settled automatically.
- [Support safe multi-replica deployments](https://github.com/rxbynerd/hairpin/issues/4) by moving live control-event routing out of process.
- Verify the sandbox path with each documented RuntimeClass; the local kind recipe currently exercises only the cluster-default runtime.
- Make the per-job event cap configurable and signal truncation on the timeline API. Each store caps a job's timeline at 10000 events — exactly in memory, approximately in Redis (`XADD MAXLEN ~`) — with no flag reaching either, and `tool_call` roughly doubles a tool-heavy run's volume, so a long run now loses its early timeline silently.
- A sandbox in `allowlist` mode reaches nothing, in-cluster services included, except through the egress proxy, so any deployment running a git profile must deploy `examples/k8s/egress-proxy.yaml` and keep its allowlist current (see [`examples/k8s/README.md`](examples/k8s/README.md#network-mode-and-the-egress-proxy)). There is no server-side check that the proxy a profile names exists.
- Run the sandbox token refresh end to end on the dev kind cluster: a `git` profile with `timeout: 1200` against the default 15-minute TTL, pushing after the first expiry, so a refreshed token is proved to reach the sandbox's credential helper. Needs a harness image carrying [stirrup PR #609](https://github.com/rxbynerd/stirrup/pull/609), which `ghcr.io/rxbynerd/stirrup:latest` picks up on the next green main build. Never exercised — every live run so far finished inside one token's lifetime ([`docs/e2e-live-diary.md`](docs/e2e-live-diary.md)).

## Protocol coverage

- [Reject unsupported RunConfig capabilities during submission](https://github.com/rxbynerd/hairpin/issues/1) instead of allowing a job to reach a protocol request Hairpin cannot answer.
- Narrow `haybale-policy` from the `hp-*` ceiling to per-caller rules. `scripts/dev/haybale-github.sh` widens it further still, to every repository under one GitHub owner.
- Correlate `tool_call.id` with `tool_result.tool_use_id` in the web UI's timeline and mark a call still open at `done` as orphaned. Hairpin records both events verbatim and joins nothing ([`docs/api.md`](docs/api.md#tool-calls-and-results)); today a reader pairs them by eye.
- Verify hairpin's retention and cancellation paths against a harness carrying [stirrup PR #608](https://github.com/rxbynerd/stirrup/pull/608): confirm jobs settle on their real `done.stop_reason` rather than the crash-inferred path when the harness closes cleanly.
- Rotate the sandbox-token signing key without a haybale restart; haybale reads the JWKS file once at startup.
- Add follow-up turns and batch execution only with end-to-end lifecycle and cancellation semantics. Asynchronous tool results are answered for the two memory tools only.
- [Reconcile permission state after harness-side timeouts](https://github.com/rxbynerd/hairpin/issues/2) so a late API response cannot appear effective after the harness has moved on.

## Shared memory

Memory ([`docs/memory.md`](docs/memory.md)) runs on published images. Both upstream dependencies are merged: the `tools.controlPlane` RunConfig surface in stirrup, and Billet's container image.

- Recall and save memory without the model's cooperation: search at task start and save an outcome at `done`, rather than relying on the tool descriptions steering the model to call them.
- Partition memory per profile or per caller instead of one Billet namespace per deployment, once Billet's contract allows a caller-supplied namespace.
- Authenticate hairpin to Billet, so the NetworkPolicy is not the only access control on the memory store.

## Security and observability

- [Add authentication, authorization, and transport security](https://github.com/rxbynerd/hairpin/issues/5) for the API, UI, and harness control plane.
- Instrument the web UI's HTTP handlers, and the Redis and Kubernetes
  clients, so store and launcher latency is more than the enclosing
  span's duration. Service traces and metrics themselves are built
  ([`docs/observability.md`](docs/observability.md)).
- Correlate a run's own trace with hairpin's spans. A run's
  `trace_emitter` is defaulted from `-harness-telemetry-endpoint`
  rather than named per profile, matching how the `-sandbox-*` flags
  fill in executor coordinates, but hairpin propagates no trace context
  into the harness, so the two traces meet only on the job ID.

Current operational constraints and unsupported protocol events are
documented in [`docs/design.md`](docs/design.md#current-limitations),
[`docs/api.md`](docs/api.md#unsupported-protocol-capabilities), and
[`docs/deployment.md`](docs/deployment.md#operational-notes).
