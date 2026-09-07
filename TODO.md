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
- Reach haybale from a sandbox on a NetworkPolicy-enforcing CNI: deploy stirrup's `examples/k8s/egress-proxy/` into the sandbox namespace and switch the `git` profile to `allowlist` mode, or add an in-cluster-services network mode to stirrup. Until then `git-smoke-test` cannot pass on kind (see [`examples/k8s/README.md`](examples/k8s/README.md#network-mode-a-known-gap-not-a-silent-one)).
- Point haybale at a GitHub App upstream instead of the development gitea, and narrow `haybale-policy` from the `hp-*` ceiling to per-caller rules.
- Rotate the sandbox-token signing key without a haybale restart; haybale reads the JWKS file once at startup.
- Add follow-up turns and batch execution only with end-to-end lifecycle and cancellation semantics. Asynchronous tool results are answered for the two memory tools only.
- [Reconcile permission state after harness-side timeouts](https://github.com/rxbynerd/hairpin/issues/2) so a late API response cannot appear effective after the harness has moved on.

## Shared memory

Memory ([`docs/memory.md`](docs/memory.md)) depends on two upstream changes that are not yet merged: [stirrup PR #586](https://github.com/rxbynerd/stirrup/pull/586) for the `tools.controlPlane` RunConfig surface and [billet PR #1](https://github.com/rxbynerd/billet/pull/1) for a published Billet image. Re-vendor the harness proto from stirrup main once the first merges.

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
- The reference `steeplechase.yaml` names
  `ghcr.io/rxbynerd/steeplechase:latest`, which is not published (a
  pull returns 403), and building it locally needs a fix to its own
  Dockerfile, which pins an older Go toolchain than its `go.mod`
  requires. Until then, `scripts/dev/deploy.sh` treats the collector as
  optional.

Current operational constraints and unsupported protocol events are
documented in [`docs/design.md`](docs/design.md#current-limitations),
[`docs/api.md`](docs/api.md#unsupported-protocol-capabilities), and
[`docs/deployment.md`](docs/deployment.md#operational-notes).
