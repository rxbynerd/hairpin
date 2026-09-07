# Roadmap

Hairpin's current implementation is Kubernetes-native, supports one run
per job, and requires a single replica. Future work
is tracked in GitHub rather than as a session log:

## Reliability and operations

- [Add job retention and stale-job reconciliation](https://github.com/rxbynerd/hairpin/issues/3), including jobs that never dial back or lose their harness.
- [Support safe multi-replica deployments](https://github.com/rxbynerd/hairpin/issues/4) by moving live control-event routing out of process.
- Verify the sandbox path with each documented RuntimeClass; the local kind recipe currently exercises only the cluster-default runtime.

## Protocol coverage

- [Reject unsupported RunConfig capabilities during submission](https://github.com/rxbynerd/hairpin/issues/1) instead of allowing a job to reach a protocol request Hairpin cannot answer.
- Add sandbox identity token issuance, including audience validation, key rotation/JWKS publication, short-lived per-run claims, and policy-scoped identities.
- Add follow-up turns and batch execution only with end-to-end lifecycle and cancellation semantics. Asynchronous tool results are answered for the two memory tools only.
- [Reconcile permission state after harness-side timeouts](https://github.com/rxbynerd/hairpin/issues/2) so a late API response cannot appear effective after the harness has moved on.

## Shared memory

Memory ([`docs/memory.md`](docs/memory.md)) depends on two upstream changes that are not yet merged: [stirrup PR #586](https://github.com/rxbynerd/stirrup/pull/586) for the `tools.controlPlane` RunConfig surface and [billet PR #1](https://github.com/rxbynerd/billet/pull/1) for a published Billet image. Re-vendor the harness proto from stirrup main once the first merges.

- Recall and save memory without the model's cooperation: search at task start and save an outcome at `done`, rather than relying on the tool descriptions steering the model to call them.
- Partition memory per profile or per caller instead of one Billet namespace per deployment, once Billet's contract allows a caller-supplied namespace.
- Authenticate hairpin to Billet, so the NetworkPolicy is not the only access control on the memory store.

## Security and observability

- [Add authentication, authorization, and transport security](https://github.com/rxbynerd/hairpin/issues/5) for the API, UI, and harness control plane.
- [Add OpenTelemetry instrumentation](https://github.com/rxbynerd/hairpin/issues/6) for submit, launch, stream lifecycle, outcomes, and latency.

Current operational constraints and unsupported protocol events are
documented in [`docs/design.md`](docs/design.md#current-limitations),
[`docs/api.md`](docs/api.md#unsupported-protocol-capabilities), and
[`docs/deployment.md`](docs/deployment.md#operational-notes).
