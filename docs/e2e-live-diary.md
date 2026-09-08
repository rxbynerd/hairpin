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

## 2026-09-08 23:00: redeploy onto the existing cluster

Commits `78a4a0c`, `0bdc745`, `d2aac15` on `e2e-fixes` carry the
changes below.

- `deploy.sh` ran with `BILLET_DIR` and `STEEPLECHASE_DIR` pointed at
  nowhere so the published images were pulled. `steeplechase:main`
  pulled and rolled out on the first try; the collector no longer needs
  a hand-built image. The egress proxy Deployment in `hairpin-sandboxes`
  came up from `stirrup:latest` and logged
  `egress proxy listening ... allowlist_entries=1`.
- The harness image is back to the default `ghcr.io/rxbynerd/stirrup:latest`;
  the `localhost/stirrup:cp-tools` patch is gone.
- `provider.sh cerebras https://api.cerebras.ai/v1 qwen-3.8-27b` ran
  from the tmux session with the key in `HAIRPIN_PROVIDER_API_KEY`, so
  1Password was never prompted again. hairpin logged
  `memory tools enabled` and `sandbox identity token issuance enabled`
  on restart.
- `haybale.sh` with no checkout pulled `haybale:latest`. The `git`
  profile it writes now uses `allowlist` mode through the proxy.
- `haybale-github.sh` stored the App key, and haybale started with
  `upstreams=2`: the init container's `0600` copy passed the
  permission check on the first attempt.

## 2026-09-08 23:08: baseline smoke tests

`smoke-test.sh` passed on the published harness image in 6 s, with a
warning that steeplechase logged no run block within 60 s. That
warning is misleading: steeplechase's log shows the spans of later
runs arriving live, so delivery works and the check is racing the
collector's grouped flush. Not chased further.

`git-smoke-test.sh` (gitea through haybale, now in `allowlist` mode)
**failed**: the job reported success, but the seed repo's `main` did
not move. The harness log shows one `run_command` that ran for exactly
60 s, and the recorded `tool_result` is `Cloning into '/tmp/repo'...`
followed by `[timed out after 60s]`. Neither haybale nor the egress
proxy logged a request, so the sandbox never reached the proxy.

**Root cause, verified in the sandbox image.** A throwaway Pod in
`hairpin-sandboxes` under a copy of the harness's allowlist
NetworkPolicy reaches haybale through the proxy Service and is
blocked going direct, so the policy and the proxy are correct. In the
published sandbox image, `git ls-remote http://haybale...` with only
`HTTP_PROXY` set hangs (exit 124 under `timeout`), and with lowercase
`http_proxy` it reaches haybale and is asked for credentials. libcurl,
hence git, ignores uppercase `HTTP_PROXY` for plain-http destinations
and stirrup's executors inject only the uppercase names (`proxyEnvFor`
in `k8s_netpol.go`, and the container executor). Every git-over-haybale
run in allowlist mode therefore hangs until the tool timeout. The fix
is in stirrup (inject both spellings); a subagent is preparing it on
`fix/lowercase-proxy-env` in a stirrup worktree, and the harness image
built from it will be loaded into kind until the change is published.

Also seen in that harness log: `failed to upload metrics: context
canceled` at exit, the harness's OTLP flush losing the race with its
own shutdown after `done`.

## 2026-09-08 23:13: first live model run

`cerebras` profile, prompt asking for OS, architecture, user, and the
presence of git, curl, python3, node. **Succeeded** in 13 s wall
(8 turns, 17 tool results), answered correctly: Debian 13 on aarch64,
`nonroot` 65532, git 2.47.3, no curl, python3, or node.

- **stirrup's tool guard rejected six of the seventeen commands.**
  `security.GuardToolCall` runs unconditionally on every `run_command`
  and rejects any command containing `curl`, `wget`, `nc`, `netcat`,
  `ncat`, or `socat` as a word, or any backtick or `$(`. Asking about
  curl guaranteed rejections (`command -v curl`, `which curl`), and the
  model's first compound command tripped the shell-escape rule. The
  model adapted, listing `/usr/bin` instead. `git` is not on the list.
  A model that writes `git commit -m "$(...)"` will be rejected.
- **The hairpin timeline never shows a tool's input.** stirrup's proto
  documents a `tool_call` event, but the core emits only `text_delta`
  and `tool_result` to the transport (`core/types.go`), so hairpin
  records results with no calls. Auditing a run means reading the
  harness Pod log. Worth an upstream issue.
- **Reasoning-model text.** qwen emits `\n\n` content alongside each
  tool call; `final_text` accumulates those, so it opens with dozens of
  blank lines before the real answer.
- steeplechase received the run's spans live, including
  `tool.failure_category=security_guard_denied`, and named the quirks
  applied: `OpenAI-compatible: native tool_choice`.
- **The WatchJob curl example in `docs/api.md` never worked**: the
  streaming method refuses `application/json` with 415. Fixed the doc
  to explain the Connect framing and point shell users at the SSE
  feed, which is what the run helper now uses.

## 2026-09-08 23:15: memory round trip, first half

`cerebras` profile: search memory, inspect git and curl, save a fact.
**Succeeded** in 43 s. `search_memory` returned four records left by
the fake-provider smoke tests, and `save_memory` returned
`{"accepted": true}` with a memory ID. The model wrote in its final
text that "the guard appears to block any command string containing
curl", so the quirk is visible enough for a model to route around.

## 2026-09-08 23:16: memory recall, permissions, cancellation

- **Memory round trip closed.** A second `cerebras` job asked for the
  git version an earlier session recorded, with no shell allowed.
  `search_memory` returned the record saved a minute earlier and the
  model quoted it verbatim, including the guard note. 9 s wall.
- **`ask-upstream` works with a live model.** A `run_config_json`
  submit (same provider, `permissionPolicy: ask-upstream`, timeout
  90 s) produced four `permission_request` events, one per
  `run_command`. The first was denied through `AnswerPermission` with
  the reason "use hello.txt instead"; the model read the refusal,
  switched file names, and the run succeeded in 18 s.
  `ListPermissionRequests` shows all four with their inputs and the
  denial reason, so the permission record is the one place in
  hairpin's own store where a tool's input is visible.
- **Cancellation lands at the turn boundary, as documented.** A
  counting job was cancelled after its first tool result; the model
  had batched ten parallel `run_command` calls into one turn, so the
  harness finished all ten (each with `sleep 2`) before reporting
  `done` with `stop_reason: cancelled`. A profile that allows parallel
  tool calls makes cancel latency proportional to the batch.

## 2026-09-08 23:20: the proxy env fix, verified

The subagent's stirrup branch `fix/lowercase-proxy-env` (commit
`16b98ed7`, off stirrup main `e7adc573`) injects `http_proxy`,
`https_proxy`, and `no_proxy` beside the upper-case names in both
executors and reserves them in `sandboxidentity`. Its `just test` and
`just lint` were green. The harness image built from it was loaded into
kind as `localhost/stirrup:proxy-env` and hairpin's Deployment patched
to `--harness-image=localhost/stirrup:proxy-env`.

**`git-smoke-test` passed**: job succeeded and gitea's `main` moved
from `4eb336a8` to `70dcf41c`. That is the first time the sandbox to
egress proxy to haybale to git host chain has completed on kind. The
branch is pushed and opened as a stirrup pull request.

The steeplechase warning from the earlier smoke test is confirmed as
timing only: its log holds a grouped `=== run hp-... started/finished`
block for that job; `kubectl logs` on the kind-on-podman node lags.
