# TODO / progress

Working session log so any future session can pick this up. Keep this
current: check items off as they land, add discoveries.

## Wave 0 — scaffold (main session)
- [x] Study stirrup protocol + conventions (see docs/design.md)
- [x] Vendor proto/harness/v1/harness.proto from stirrup; write proto/hairpin/v1/hairpin.proto
- [x] buf.yaml / buf.gen.yaml (managed mode, connect-go), `just proto` generates into gen/
- [x] go.mod with all deps pre-resolved (subagents must NOT touch go.mod/go.sum)
- [x] internal/job types, Store interface + memstore, Launcher interface, registry
- [x] cmd/hairpin skeleton compiling end-to-end; Justfile; initial commits (84feabe)
- [x] Wave 1 dispatched: 5 parallel feature-implementers (redisstore, controlplane, service+api, launchers, web)

## Wave 1 — parallel implementation (all landed, committed de15990..478d6b3)
- [x] internal/store/redisstore + miniredis tests
- [x] internal/controlplane HarnessService handler + tests
- [x] internal/service + internal/api JobService handler + tests
- [x] internal/launcher k8s + process impls + examples/k8s manifests (manifests not yet applied to a live cluster)
- [x] internal/web embedded UI + SSE

## Wave 2 — integration (done, 6225392 + d94cf06)
- [x] Wire cmd/hairpin (native net/http unencrypted-HTTP/2, no x/net h2c)
- [x] Wire-level e2e tests (fake harness over real gRPC): full loop, cancel-before-harness, crash settlement. Found+fixed: empty profile set rejected default-profile flag.

## Wave 3 — review & docs
- [x] change-verifier: real-binaries run found THE critical bug — finish() wrote terminal state on the stream's context, which the exiting harness cancels first; every completed job stuck RUNNING forever. Fixed (d76b07e): pump context detached via context.WithoutCancel + regression test over a ctx-respecting store wrapper (proven failing pre-fix). Also fixed: "warning" event handled, launcher logs workdir.
- [x] Main session re-verified live post-fix with real stirrup + redis + fake OpenAI SSE provider: success path (SUCCEEDED, stop_reason "success", finalText captured), failure path (FAILED with harness error), token session flow through real binary, SSE timeline, UI badges, redis layout — all correct; processes cleaned up.
- [x] code-reviewer findings triaged: its CRITICAL ("success" is not a stop_reason) was a FALSE POSITIVE — verified against stirrup source (loop.go emits done.StopReason=outcome, happy path literally "success"); all real findings fixed in 63c9f51
- [x] security-reviewer findings fixed (63c9f51): per-job harness session tokens (SubmitJobResponse.harness_session = "<id>.<token>" via CONTROL_PLANE_SESSION_ID), executor.type now required at submit, pod hardening, 4MiB read caps, permission cap, SSE type demotion, same-origin+headers on UI, shutdown cancels live runs, AnswerPermission persist-first, id format validation
- [x] docs agent: README, docs/api.md, docs/deployment.md written; second pass updating for the fix wave in flight
- [x] Commit docs pass; final full-suite run

## Wave 4 — Kubernetes-native deployment (done, 516a0a6 + 20ab574)
- [x] Dropped the process launcher (`--stirrup-bin` path-to-binary); launcher is
      `kubernetes` (default) or `none`. `k8s-` flag prefix gone.
- [x] Images defaulted to the published ghcr.io/rxbynerd/stirrup and
      stirrup-sandbox tags; namespace read from the projected ServiceAccount;
      `--advertise` defaults to hairpin.<ns>.svc:<port>. In-cluster hairpin needs
      no flags beyond --redis/--profiles/identities.
- [x] Harness Job now mounts its ServiceAccount token (was pinned false, which
      would have failed every sandboxed run at executor construction).
- [x] hairpin owns sandbox coordinates: `--sandbox-image/-namespace/-service-account/-runtime`
      are injected into each submitted RunConfig's k8s/k8s-sandbox executor;
      stirrup's cross-field rules mirrored at submit.
- [x] examples/k8s: two namespaces (hairpin + hairpin-sandboxes), three
      identities, profiles ConfigMap. Containerfile USER made numeric (65532) —
      runAsNonRoot rejects a non-numeric image user.
- [x] scripts/dev: kind-up/deploy/smoke-test/kind-down + a fake OpenAI-SSE
      provider. VERIFIED end to end on kind+podman: harness Job → sandbox Pod +
      NetworkPolicy in hairpin-sandboxes → run_command via pods/exec as uid
      65532 → SUCCEEDED, both torn down at end of run.

## Known deferrals (see docs/design.md "Deliberately deferred")
Follow-ups, sandbox tokens (explicit refusal), batch, multi-replica, auth.

## Backlog (post-v1, discovered during build)
- Job/event retention: no TTL/reaper exists — event streams are count-capped
  (MAXLEN ~10000) but job hashes/permissions/index grow forever. Documented
  honestly in docs/deployment.md; needs a real reaper or key TTLs.
- gVisor RuntimeClass untested: the kind cluster installs none, so
  `--sandbox-runtime gvisor` is unexercised. stirrup's scripts/dev/kind-up.sh
  installs runsc if that path needs proving.
- Reaper for jobs stuck in awaiting_harness (pod never dialled back).

## Notes for future sessions
- stirrup checkout: ~/Developer/stirrup (built arm64 binary at repo root)
- Correlation: CONTROL_PLANE_SESSION_ID env → echoed in ready.id
- `kind get clusters` fails on this podman (Go template over .Labels); the dev
  scripts check for the `<cluster>-control-plane` container instead. `kind create`
  and `kind load image-archive` work fine with KIND_EXPERIMENTAL_PROVIDER=podman.
- Wire RunConfig needs explicit run_id/mode/prompt/provider.type/max_turns/timeout (+permission_policy for execution; +tools.built_in for read-only modes) — no CLI defaults on the wire
