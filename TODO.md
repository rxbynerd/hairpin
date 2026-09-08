# Roadmap

Hairpin's current implementation is Kubernetes-native, supports one run
per job, and requires a single replica. Future work
is tracked in GitHub rather than as a session log:

## Reliability and operations

- [Add job retention and stale-job reconciliation](https://github.com/rxbynerd/hairpin/issues/3): `--retention` deletes terminal jobs past the window and the same reaper fails jobs that never dial back. Still open for a job that loses its harness mid-run, which is reported through `last_event_at` but never settled automatically.
- [Support safe multi-replica deployments](https://github.com/rxbynerd/hairpin/issues/4) by moving live control-event routing out of process.
- Verify the sandbox path with each documented RuntimeClass; the local kind recipe currently exercises only the cluster-default runtime.

## Protocol coverage

- [Reject unsupported RunConfig capabilities during submission](https://github.com/rxbynerd/hairpin/issues/1) instead of allowing a job to reach a protocol request Hairpin cannot answer.
- Wait on stirrup injecting lowercase `http_proxy`/`https_proxy`/`no_proxy` beside the uppercase names (branch `fix/lowercase-proxy-env`). git, through libcurl, honours only the lowercase spelling for plain-http URLs, so a clone through a plain-HTTP haybale in `allowlist` mode hangs until the tool timeout and `git-smoke-test` needs a harness image built from that branch (see [`examples/k8s/README.md`](examples/k8s/README.md#network-mode-and-the-egress-proxy)).
- Narrow `haybale-policy` from the `hp-*` ceiling to per-caller rules. `scripts/dev/haybale-github.sh` widens it further still, to every repository under one GitHub owner.
- stirrup's harness emits no `tool_call` event on the control-plane stream, only `tool_result`, so a run's timeline holds results with no inputs and auditing a call means reading the harness Pod log. Hairpin records the event when it arrives; emitting it is upstream.
- Rotate the sandbox-token signing key without a haybale restart; haybale reads the JWKS file once at startup.
- Add follow-up turns and batch execution only with end-to-end lifecycle and cancellation semantics. Asynchronous tool results are answered for the two memory tools only.
- [Reconcile permission state after harness-side timeouts](https://github.com/rxbynerd/hairpin/issues/2) so a late API response cannot appear effective after the harness has moved on.

## Shared memory

Memory ([`docs/memory.md`](docs/memory.md)) runs on published images: `ghcr.io/rxbynerd/stirrup:latest` carries the `tools.controlPlane` RunConfig surface from [stirrup PR #586](https://github.com/rxbynerd/stirrup/pull/586), and `ghcr.io/rxbynerd/billet:latest` pulls.

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
