# Live end-to-end diary

A running log of the first live-fire end-to-end test of the
Equestrianism components through hairpin: a real model provider, a
real GitHub App, the published container images, and a real private
repository as the target of an agent run. Entries are chronological;
each records what was tried, what was observed, and what was changed
in response. Future agents extending the test should append, not
rewrite.

Fixes made along the way ship on the `e2e-fixes` branch.

## Fixed coordinates

| Thing | Value |
|---|---|
| Model provider | Cerebras Cloud, `https://api.cerebras.ai/v1`, model `qwen-3.8-27b` (openai-compatible) |
| Provider key | 1Password `op://Private/Cerebras/credential`, loaded once into a tmux session named `e2e` as `CEREBRAS_API_KEY` |
| GitHub App | `haybale-dev`, app ID 4278664, client ID `Iv23liqG0dNT1nrZHZaQ`; permissions `contents: write`, `metadata: read`; installed on `rxbynerd` and `ghostworks` (all repositories) |
| App private key | `~/Downloads/haybale-dev.2026-09-08.private-key.pem` |
| Target repository | `github.com/rxbynerd/springboard-chrome` (private): tidy the debugging in `springboard.js` |
| Cluster | The existing `kind-hairpin` cluster on podman (up 7 days at start) |

## 2026-09-08 evening: survey before touching anything

Findings that shaped the plan, in the order they were established.

**Upstream state has moved since the docs were written.**

- stirrup PR #586 (`tools.controlPlane`) merged on 2026-09-02, so
  `ghcr.io/rxbynerd/stirrup:latest` now carries the control-plane tool
  surface and the `localhost/stirrup:cp-tools` patch the dev cluster
  relied on is obsolete. Hairpin's vendored proto is byte-identical to
  stirrup main.
- Published images, probed anonymously against the GHCR registry API:
  `stirrup:latest`, `stirrup-sandbox:latest`, `billet:latest`, and
  `haybale:latest` all resolve. `steeplechase:latest` does not exist:
  its CI publishes `edge`, `main`, and `sha-<short>` tags only, and
  its Containerfile was fixed (renamed from `Dockerfile`), so
  `deploy.sh`'s `Dockerfile` probe and best-effort handling are stale.
- haybale's `github-app` credential type mints a per-request
  installation token scoped to one repository and one permission
  (`contents: read` for fetch, `contents: write` for push). Its
  private-key loader refuses any file with group or other permission
  bits, which rules out a plain Secret mount under a non-root
  container (a Secret volume with `fsGroup` lands at `0440`).

**The provider works, with quirks worth watching.** A direct
chat-completions call with a `run_command` tool produced a
`finish_reason: tool_calls` response with a well-formed arguments
object. Two things stirrup's openai-compatible provider does not
model: the response carries a top-level `reasoning` string on the
message (not `reasoning_content`), and tool-call IDs are short
nine-character hex strings rather than `call_...`. Queue time on the
first call was 3.6 s against 0.04 s of generation. stirrup's quirk
registry has entries for OpenAI o-series, gpt-5, DeepSeek, and
Gemini; nothing matches `qwen-*`.

**The GitHub App path is viable end to end.** A JWT signed with the
private key lists both installations, and a scoped installation token
for `springboard-chrome` alone mints with `contents: read`.

**The network gap is closable with what stirrup already ships.**
stirrup's egress proxy forwards non-CONNECT requests as plain HTTP
(`handleHTTP` in `egressproxy/proxy.go`), and its allowlist matcher
accepts `host:port` entries. So a sandbox in `allowlist` mode, with
`HTTP_PROXY` pointing at a `stirrup-egress-proxy` Deployment in
`hairpin-sandboxes` and `haybale.hairpin.svc:8466` in that proxy's
allowlist, reaches haybale through the proxy. haybale's own ingress
NetworkPolicy admits the sandbox namespace, which the proxy Pod lives
in. Hairpin already passes `executor.k8sEgressProxyUrl` through from a
profile (it validates the pairing with `allowlist` mode and injects
nothing), so no server change is needed for the route itself.

**Plan.**

1. Deploy the egress proxy into the sandbox namespace as a new
   reference manifest, and update the published-image references.
2. Give haybale a `github.com` upstream backed by the App, with the
   key copied into an `emptyDir` at `0400` by an init container.
3. Add `cerebras` and `cerebras-git` profiles by script, mirroring
   `openrouter.sh`, reading the key from the environment so the tmux
   session is the only place it is typed.
4. Run: fake-provider smoke test (baseline after redeploy), a plain
   Cerebras run, a memory run, then the git run against
   `springboard-chrome` pushing to a branch.
