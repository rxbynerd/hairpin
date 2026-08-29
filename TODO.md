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

## Wave 5 — real-model verification (done, 9c03430)
- [x] End-to-end against a real model: `scripts/dev/openrouter.sh` / `just
      openrouter <op-ref>` wires an OpenRouter key (1Password) into
      provider-api-keys plus an "openrouter" profile
      (openai-compatible → https://openrouter.ai/api/v1,
      google/gemini-3.7-flash). Two live jobs ran the full agentic loop
      (multi-turn run_command in the sandbox) to JOB_STATUS_SUCCEEDED.
- [x] Gotchas: profiles load once at startup, so a ConfigMap patch needs a
      hairpin restart; deploy.sh recreates the Secret and profiles ConfigMap,
      so re-run openrouter.sh after every deploy; a kubectl port-forward goes
      stale across a rollout restart.

## Wave 6 — Haybale: sandbox git access (BLOCKED: no GitHub App credentials yet)
haybale (~/Developer/haybale) is an authenticating reverse proxy for Git
smart HTTP: the sandbox authenticates to haybale with a short-lived JWT,
haybale checks a default-deny repo policy and swaps in a per-request
GitHub App installation token; the upstream credential never reaches the
sandbox. Stirrup's wire contract already supports the whole flow — no
stirrup changes needed:
- `executor.sandbox_identity {source: "control-plane", audience, env_var}`:
  after task assignment and before sandbox creation the harness sends
  `sandbox_token_request` and blocks fail-closed up to 60s for
  `sandbox_token_response`; the JWT is injected into the sandbox env
  (default `HAYBALE_TOKEN`) and never enters RunConfig or traces. Wire
  contract: stirrup docs/deployment.md#sandbox-identity-token-issuance.
- `executor.git_proxy {url, hosts, rewrite_ssh, token_env_var}`: composes
  non-secret GIT_CONFIG_* env rewriting e.g. github.com through haybale.
  In allowlist network mode the proxy host:port must be in
  network.allowlist.

Jobs to be done:
- [ ] hairpin as JWT issuer: signing key (ES256), JWKS served over HTTP for
      haybale's `jwksURL`, per-run token (sub = run identity, short TTL,
      aud from config), answer `sandbox_token_response` — replace the
      explicit refusal in internal/controlplane (pump.go
      refuseSandboxToken / sandboxTokenRefusal).
- [ ] Decide the repo-scope surface: haybale narrows YAML policy with a
      `repoScopeClaim` (e.g. haybale.dev/repos) — probably a profile field
      and/or SubmitJobRequest addition, injected into the token.
- [ ] Deploy haybale in-cluster: image + haybale.yaml + policy.yaml
      manifests (examples/k8s or scripts/dev); default-deny policy keyed on
      run identities. haybale has docs/stirrup-integration.md and a live
      GitHub App acceptance runbook.
- [ ] Profiles: executor.sandbox_identity + executor.git_proxy + network
      mode "allowlist" with haybale's host:port.
- [ ] BLOCKED on GitHub App credentials for the upstream side. Until then a
      dev-cluster path exists: haybale supports static credentials for
      non-GitHub hosts, so an in-cluster git host (e.g. gitea) could prove
      the token flow end to end.

## Wave 7 — Steeplechase: telemetry out of the cluster
steeplechase (~/Developer/steeplechase) is a single-binary OTLP router:
receives OTel metrics/logs/traces on :4317 (gRPC) / :4318 (HTTP), fans
out to sinks (stdout, otlp+grpc/http/https, mqtt), admin/healthz/metrics
on :9090, per-run grouped stdout keyed on run.id. Stirrup emits
`stirrup.harness.*` metrics and a run/turn/tool_call span tree when
RunConfig.trace_emitter = {type: "otel", endpoint, protocol, headers
(secret:// resolved), capture_content}; its slog goes to stderr, not OTLP
logs.

Jobs to be done:
- [ ] Deploy steeplechase in the hairpin namespace (Dockerfile in its
      repo); dev-cluster default sink stdout so `kubectl logs` shows runs.
- [ ] Wire stirrup runs to it: traceEmitter in profiles, or (matching the
      sandbox-coordinate pattern) a hairpin `--telemetry-endpoint` flag
      injected into submitted RunConfigs where the profile left it empty —
      decide which; the harness Pod (hairpin ns) has open egress so no
      NetworkPolicy change needed.
- [ ] Instrument hairpin itself: no OTel exists in hairpin today — add
      OTLP export (traces around submit/launch/control-plane stream,
      metrics for job outcomes/durations) pointed at the same endpoint.
- [ ] Out-of-cluster publishing: configure steeplechase --sink
      (otlp+grpc://... or mqtt://...) at the external backend; secrets for
      sink headers/passwords via its own Secret.

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
