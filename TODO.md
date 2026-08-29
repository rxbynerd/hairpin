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
- Add follow-up turns, batch execution, and asynchronous tool-result handling only with end-to-end lifecycle and cancellation semantics.
- [Reconcile permission state after harness-side timeouts](https://github.com/rxbynerd/hairpin/issues/2) so a late API response cannot appear effective after the harness has moved on.

## Security and observability

- [Add authentication, authorization, and transport security](https://github.com/rxbynerd/hairpin/issues/5) for the API, UI, and harness control plane.
- [Add OpenTelemetry instrumentation](https://github.com/rxbynerd/hairpin/issues/6) for submit, launch, stream lifecycle, outcomes, and latency.

Current operational constraints and unsupported protocol events are
documented in [`docs/design.md`](docs/design.md#current-limitations),
[`docs/api.md`](docs/api.md#unsupported-protocol-capabilities), and
[`docs/deployment.md`](docs/deployment.md#operational-notes).
