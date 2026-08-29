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

## Wave 3 — review & docs (dispatched, in flight)
- [ ] change-verifier: real binaries (hairpin + stirrup + redis-server + stub provider) live run
- [ ] code-reviewer findings → fix
- [ ] security-reviewer findings → fix (reports land in .claude/reviews/)
- [ ] docs agent: README, docs/api.md, docs/deployment.md
- [ ] After fixes: re-run full suite, final commits

## Known deferrals (see docs/design.md "Deliberately deferred")
Follow-ups, sandbox tokens (explicit refusal), batch, multi-replica, auth.

## Backlog (post-v1, discovered during build)
- Job/event retention: no TTL/reaper exists — event streams are count-capped
  (MAXLEN ~10000) but job hashes/permissions/index grow forever. Documented
  honestly in docs/deployment.md; needs a real reaper or key TTLs.
- examples/k8s manifests unverified against a live cluster (local kind+podman
  integration broken); smoke-test before relying on them.
- Reaper for jobs stuck in awaiting_harness (pod never dialled back).

## Notes for future sessions
- stirrup checkout: ~/Developer/stirrup (built arm64 binary at repo root)
- Correlation: CONTROL_PLANE_SESSION_ID env → echoed in ready.id
- Wire RunConfig needs explicit run_id/mode/prompt/provider.type/max_turns/timeout (+permission_policy for execution; +tools.built_in for read-only modes) — no CLI defaults on the wire
