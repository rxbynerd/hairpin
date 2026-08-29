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

## Wave 1 — parallel implementation (subagents; disjoint package ownership)
- [ ] internal/store/redisstore + miniredis tests (owner: redis-store agent)
- [ ] internal/controlplane HarnessService handler + tests (owner: control-plane agent)
- [ ] internal/service + internal/api JobService handler + tests (owner: api agent)
- [ ] internal/launcher k8s + process impls + examples/k8s manifests (owner: launcher agent)
- [ ] internal/web embedded UI + SSE (owner: web agent)

## Wave 2 — integration (main session)
- [ ] Wire cmd/hairpin fully (config → store → registry → controlplane + api + web on one h2c listener)
- [ ] End-to-end test: hairpin + real redis (or miniredis) + real ~/Developer/stirrup/stirrup binary via process launcher

## Wave 3 — review & docs
- [ ] code-reviewer + security-reviewer wave, fix findings
- [ ] change-verifier e2e pass
- [ ] README, docs/deployment.md (K8s recipe), docs/api.md

## Known deferrals (see docs/design.md "Deliberately deferred")
Follow-ups, sandbox tokens (explicit refusal), batch, multi-replica, auth.

## Notes for future sessions
- stirrup checkout: ~/Developer/stirrup (built arm64 binary at repo root)
- Correlation: CONTROL_PLANE_SESSION_ID env → echoed in ready.id
- Wire RunConfig needs explicit run_id/mode/prompt/provider.type/max_turns/timeout (+permission_policy for execution; +tools.built_in for read-only modes) — no CLI defaults on the wire
