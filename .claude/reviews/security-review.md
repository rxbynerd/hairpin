# Hairpin security review

Scope: all code new since commit `0dd6840` (the entire hairpin repo).
Reviewed with `docs/design.md`'s v1 trust posture as baseline — plaintext,
unauthenticated transport on a trusted network, single replica, no
API/UI auth. That posture itself is out of scope per the review brief.
Findings below are things that make the posture *worse* than documented,
or that would bite an operator even inside a fully trusted network.

Reviewed: `internal/controlplane`, `internal/registry`, `internal/launcher`
(k8s + process), `internal/service`, `internal/api`, `internal/store`
(memstore + redisstore), `internal/web` (handlers, sse, render, view,
templates, static/app.js), `internal/config`, `cmd/hairpin`,
`examples/k8s/*.yaml`, vendored `proto/harness/v1/harness.proto`,
`proto/hairpin/v1/hairpin.proto`.

---

## Findings

### [HIGH] Any stream can hijack any job's session by echoing its ID — no possession proof

- **File:** `internal/controlplane/controlplane.go:161-208`, `internal/registry/registry.go`
- **Description:** The only thing that binds an inbound `RunTask` stream to a
  job is the `ready.id` field the *client* sends — a plain string the
  connecting harness controls entirely. `runTask` looks up the job by that
  ID (`h.store.GetJob(ctx, jobID)`), and if the job exists, isn't terminal,
  and no session is already registered, it hands over the job's full
  `RunConfig` (`ctlTaskAssignment`) and starts pumping events for it. There
  is no shared secret, token, or mTLS identity tying the *launched* pod to
  the session it's allowed to claim — anything that can open a stream to
  hairpin's control-plane port and knows (or guesses) a live, unclaimed job
  ID owns that job's session.
- **Attack scenario:** A second workload on the same trusted network/mesh —
  e.g. a compromised harness Pod from a *different, legitimately-launched*
  job, or literally any pod that can reach hairpin's h2c port — opens its
  own `RunTask` stream and sends `ready.id = "hp-<victim-ulid>"` before the
  victim's real harness connects (a race that's easy to win: the victim Job
  spends time in `awaiting_harness` while K8s schedules it). The attacker's
  stream is registered as owner of the victim's session, receives the
  victim's `RunConfig` verbatim (§ `dynamicContext`, prompts, provider
  config, secret references), can emit fabricated `text_delta`/`done`/
  `permission_request` events that get persisted to the victim's timeline
  and surfaced in the web UI/API as if from the real run, and can terminate
  the job with an arbitrary `stop_reason`. This is lateral movement *between
  jobs* purely via the shared control plane — it doesn't require compromising
  hairpin itself, just reaching its port, which every harness Pod already
  does by design (`CONTROL_PLANE_ADDR`).
- **Blast radius of the ID:** job IDs are `hp-<ulid>` (`internal/job/job.go:78-80`).
  ULIDs are *not* unpredictable — they're `timestamp (48 bits) + randomness
  (80 bits)`, generated with `crypto/rand`, so the random component alone is
  unguessable in isolation, but the ID is not a secret in this system's
  actual use: it appears in the K8s Job/Pod name (`internal/launcher/k8s.go:85`,
  `l.jobLabels`), in `CONTROL_PLANE_SESSION_ID` env var (visible via
  `kubectl describe pod`/`/proc/<pid>/environ` to anything with pod exec/describe
  access), in the web UI URL (`/jobs/{id}`), in `SubmitJob`'s response, and in
  `kubectl get jobs` output. Any of those observation points — not brute
  force — is enough to hijack a session, which is a materially larger attack
  surface than "ULIDs are crypto-random so this is fine."
- **Evidence:**
  ```go
  // internal/controlplane/controlplane.go
  jobID := first.GetId()
  ...
  j, err := h.store.GetJob(ctx, jobID)
  ...
  sess := &session{s: s}
  if err := h.reg.Register(jobID, sess); err != nil { ... }
  ...
  sess.Send(&harnessv1.ControlEvent{Type: ctlTaskAssignment, Task: cfg})
  ```
  No credential, no mTLS peer identity, no comparison against anything the
  launcher itself set — `Register` only rejects a *second* claim of an
  *already-claimed* session (`registry.go:41-49`), it does nothing to
  authenticate the *first* claim.
- **Confidence:** high — this is the entire correlation mechanism, verified
  by reading `runTask`, `registry.Register`, and the launcher contract
  comment in `internal/launcher/launcher.go:14-20` which states outright:
  "the harness echoes the session ID in its ready event, **which is the
  only correlation** between a launched workload and an inbound stream."
- **Remediation:** Mint a per-job, single-use, unguessable bearer credential
  at `Submit` time (e.g. a random 128-bit token stored alongside the job,
  never surfaced via any API/UI/label/env-visible-to-other-pods path except
  the one launched Pod's env), pass it to the launched harness as a second
  env var (e.g. `CONTROL_PLANE_SESSION_TOKEN`), and require the `ready`
  event (or a connect request header, since the harness dials in) to
  present it. Compare with `crypto/subtle.ConstantTimeCompare`. This closes
  the gap without touching stirrup's wire protocol *if* stirrup's harness
  already forwards arbitrary metadata/headers on the RunTask dial — if it
  doesn't, this needs a stirrup-side change too (worth raising upstream,
  since stirrup's own `CONTROL_PLANE_SESSION_ID`-only correlation has the
  same shape of problem for any multi-tenant control plane, not just
  hairpin).
- **CWE:** CWE-306 (Missing Authentication for Critical Function) / CWE-284
  (Improper Access Control).

---

### [HIGH] `run_config_json` submits are unvalidated for `executor.type`, defaulting untrusted agent execution onto hairpin's own harness Pod with a weak securityContext and blanket secret exposure

- **File:** `internal/service/submit.go:96-116` (validateRunConfig),
  `internal/launcher/k8s.go:83-123`, `examples/k8s/hairpin.yaml:38`,
  `examples/k8s/secret.yaml`
- **Description:** `SubmitJob` accepts a caller-supplied `run_config_json`
  and only validates `prompt`, `mode`, `provider`, `max_turns`, and
  `timeout` (`submit.go:96-116`). It never validates or constrains
  `executor.type`. Per stirrup's own CLI default
  (`harness/cmd/stirrup/cmd/runconfigflags.go:61`: `f.String("executor",
  "local", ...)`), an unset/omitted executor is `local` — meaning agent
  tool calls run as literal shell commands on whatever process is running
  `stirrup job`, i.e. **hairpin's own harness Pod**, not a separate
  sandboxed Pod. Compare this to stirrup's documented "hardened sandbox
  Pod" posture for the `k8s` executor
  (`stirrup/docs/executors/k8s.md`): `allowPrivilegeEscalation: false`,
  `capabilities.drop: [ALL]`, `runAsNonRoot: true`, `runAsUser: 65532`,
  `seccompProfile.type: RuntimeDefault`, `automountServiceAccountToken:
  false`. Hairpin's own harness-launcher Pod spec
  (`internal/launcher/k8s.go:99-118`) sets only `RunAsNonRoot: true` and
  `AutomountServiceAccountToken: false` — no capability drop, no
  `allowPrivilegeEscalation: false`, no seccomp profile, no
  `readOnlyRootFilesystem`, and only CPU/memory *requests*, no *limits*.
  That same Pod carries **every** secret named in
  `--k8s-env-from-secrets` via `envFrom` (`k8s.go:111`,
  `envFromSecrets`), regardless of what that specific job's RunConfig
  actually needs (`config.go:68-70`; the reference manifest
  (`examples/k8s/hairpin.yaml:38`) wires this to a single
  `provider-api-keys` Secret for *all* jobs).
- **Attack scenario:** Any caller of the (documented-as-unauthenticated)
  `JobService.SubmitJob` API submits `run_config_json` with no `executor`
  field (or `executor.type: "local"` explicitly) and a prompt engineered
  to make the agent run shell commands. Those commands execute directly
  inside hairpin's harness Pod — which has no capability drop, no seccomp
  profile, no explicit privilege-escalation block, and full read access to
  every provider secret configured for the whole deployment via `envFrom`
  (e.g. `ANTHROPIC_API_KEY` and any other keys operators add later, even
  ones unrelated to this job's provider). The attacker exfiltrates all
  configured provider credentials with `env` or `cat /proc/self/environ`,
  or pivots using whatever weak-securityContext primitives are available.
  This is strictly worse than "trusted network, no auth" — it's remote
  code execution with unnecessarily broad secret access, reachable by
  *any* caller of the documented-as-open API, not just a network-adjacent
  attacker.
- **Evidence:**
  ```go
  // internal/launcher/k8s.go
  SecurityContext: &corev1.PodSecurityContext{
      RunAsNonRoot: &runAsNonRoot,
  },
  Containers: []corev1.Container{{
      ...
      EnvFrom: envFromSecrets(l.cfg.EnvFromSecrets), // ALL configured secrets, every job
      Resources: corev1.ResourceRequirements{
          Requests: corev1.ResourceList{ /* no Limits */ },
      },
      // no container-level SecurityContext at all
  }},
  ```
  ```go
  // internal/service/submit.go — validateRunConfig never looks at cfg.Executor
  ```
- **Confidence:** medium-high. The executor-default behavior is inferred
  from stirrup's CLI flag default and its documented executor semantics,
  not from code in this repo (hairpin only forwards the RunConfig
  verbatim), so I can't verify from *this* repository alone whether
  stirrup's server-side wire-config parsing (as opposed to its CLI flag
  parsing) applies the same "local" default when `executor` is omitted
  from a protojson `RunConfig` delivered over `task_assignment` rather
  than via CLI flags. The securityContext gap and blanket `envFrom`
  exposure are verified directly and hold regardless of that ambiguity —
  even a `k8s`-executor job's *orchestrator* process (this harness Pod)
  still has every secret and a weak securityContext.
- **Remediation:**
  1. Have hairpin's `validateRunConfig` reject or force `executor.type`
     to an explicit, sandboxed value (e.g. only allow `k8s`/`k8s-sandbox`)
     unless an operator opts in to `local`/`container` via a profile flag.
  2. Harden the harness Pod's container `SecurityContext` to match
     stirrup's own sandbox bar: `AllowPrivilegeEscalation: false`,
     `Capabilities.Drop: []string{"ALL"}`, `SeccompProfile:
     &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}`,
     `ReadOnlyRootFilesystem: true` (with an `emptyDir` for any writable
     path stirrup needs), and add `Resources.Limits` alongside `Requests`.
  3. Scope secrets per-profile instead of one blanket `EnvFromSecrets` for
     every job — e.g. a `secretsFor(profile)` mapping, or per-job
     Kubernetes `Secret` projection scoped to only the provider that
     job's resolved RunConfig actually references.
- **CWE:** CWE-250 (Execution with Unnecessary Privileges), CWE-1188
  (Insecure Default Initialization of Resource), CWE-272 (Least
  Privilege Violation).

---

### [MEDIUM] No request/message size bound on any connect-go handler or SSE — memory exhaustion via oversized submits or harness events

- **File:** `cmd/hairpin/serve.go:114-118` (`hairpinv1connect.NewJobServiceHandler`,
  `controlplane.New(...).NewHTTPHandler()` — both called with no options),
  `internal/controlplane/pump.go:160-168` (`appendProto`), `internal/service/submit.go:41`
  (`protojson.Marshal(cfg)` before persisting)
- **Description:** Neither the `JobService` handler nor the harness
  `HarnessService` handler is constructed with
  `connect.WithReadMaxBytes(...)`, and the `http.Server` in `serve.go`
  sets no `MaxHeaderBytes`/body-size guard of its own (net/http has no
  default body-size cap; only `MaxHeaderBytes` defaults, and that's
  headers-only). A `SubmitJob` call with a multi-hundred-MB
  `run_config_json` is fully buffered and protojson-unmarshalled
  (`resolveConfig`, `submit.go:73`), then immediately re-marshalled
  (`submit.go:41`) and written to Redis as one hash field
  (`redisstore/jobs.go:31`). Symmetrically, on the control-plane side, any
  connected harness stream — including a hijacked one per the finding
  above — can send a single `HarnessEvent` with an arbitrarily large
  `text`/`input`/`message` field; `appendProto` (`pump.go:160`) marshals
  it whole and stores it as one Redis stream entry with no per-message
  size check (only the *stream length* is capped via `XAdd MAXLEN ~10000`
  — a single 500MB entry among those 10000 is not prevented).
- **Attack scenario:** A caller (any network principal reaching the
  documented-open API, or any harness reaching the control-plane port)
  sends one oversized request per finding — hairpin allocates the full
  payload in memory (protojson unmarshal, then remarshal, then a Redis
  round-trip), and repeated concurrent oversized requests exhaust process
  memory well before any other limit kicks in. `WatchJob`'s SSE-equivalent
  path (`internal/web/sse.go`) also has no cap on concurrent open
  connections, so combined with oversized backing events this compounds.
- **Confidence:** high for the missing bound itself (verified by absence
  of any `WithReadMaxBytes`/`MaxBytesReader` in the codebase); medium for
  practical exploitability, since actual impact depends on Redis's own
  512MB value-size ceiling and available process memory — but a DoS via
  OOM well before that ceiling is easily reachable with concurrent
  moderately-sized (tens of MB) requests.
- **Evidence:**
  ```go
  // cmd/hairpin/serve.go — no options passed to either handler constructor
  cpPath, cpHandler := controlplane.New(st, reg, controlplane.WithLogger(logger)).NewHTTPHandler()
  apiPath, apiHandler := hairpinv1connect.NewJobServiceHandler(api.New(svc))
  ```
- **Remediation:** Pass `connect.WithReadMaxBytes(N)` (a few MB is generous
  for a RunConfig; pick a number and document it) to both
  `NewHarnessServiceHandler` and `NewJobServiceHandler`. Cap individual
  `HarnessEvent` payload sizes in `appendProto` before persisting (log +
  drop or truncate, mirroring the existing `MaxFinalTextBytes` pattern).
  Add an `http.Server.ReadTimeout`/`ReadHeaderTimeout` and consider a
  global concurrent-SSE-connection cap.
- **CWE:** CWE-400 (Uncontrolled Resource Consumption), CWE-770
  (Allocation of Resources Without Limits or Throttling).

---

### [MEDIUM] Permission-request map has no cap — unbounded growth per job from harness-controlled `request_id`s

- **File:** `internal/controlplane/pump.go:106-108,176-188`,
  `internal/store/redisstore/permissions.go:15-32`,
  `internal/store/memstore.go:215-226`
- **Description:** Every `permission_request` event's `RequestID` is taken
  verbatim from the harness (`ev.GetRequestId()`, `pump.go:178`) and
  `PutPermission` (`HSET permsKey(jobID) RequestID data`) has no equivalent
  of the event stream's `XAdd MAXLEN` cap. A stream (hijacked per the first
  finding, or simply a misbehaving/malicious legitimate harness) that emits
  many `permission_request` events with distinct `request_id`s grows that
  job's `hairpin:job:<id>:perms` hash without bound — unlike
  `hairpin:job:<id>:events`, which is capped at ~10000 entries.
- **Attack scenario:** A malicious harness stream sends millions of
  `permission_request` events with unique `request_id`s (each also
  persisted to the *capped* event stream via `appendProto`, but the perms
  hash itself keeps every one). `ListPermissions` (`GetJob` detail page,
  `AnswerPermission` API) then does an unbounded `HGetAll` over that hash on
  every page load, denial-of-service-ing both Redis (memory) and the web UI
  (CPU/response size) for that job.
- **Confidence:** medium — requires either the session-hijack path above,
  or a legitimate-but-malicious/buggy harness image; not exploitable by an
  unauthenticated network observer alone.
- **Remediation:** Cap the perms hash size per job (reject/log-and-drop
  once a threshold is hit, mirroring `MaxFinalTextBytes` /
  `DefaultMaxEvents`), and/or validate `request_id` shape (bounded length,
  restricted charset) before persisting.
- **CWE:** CWE-400 (Uncontrolled Resource Consumption).

---

### [MEDIUM] `event: %s` in the SSE stream is built from a fully attacker(harness)-controlled, unvalidated string — CRLF/frame injection

- **File:** `internal/web/sse.go:83-94` (`writeSSEEvent`), vendored
  `proto/harness/v1/harness.proto:134` (`string type = 1;` — no format
  constraint)
- **Description:** `writeSSEEvent` writes `event: %s\n` directly from
  `ev.Type`, which for most event kinds is `ev.GetType()` off the harness's
  own `HarnessEvent.type` — a free-form proto3 `string` with no charset or
  length constraint enforced anywhere in `internal/controlplane/pump.go`.
  Neither the control-plane pump (which stores `store.Event{Type:
  ev.GetType(), ...}` for unknown event types at `pump.go:131`) nor
  `writeSSEEvent` strips or rejects control characters. A `Type` containing
  `\n`/`\r` breaks the SSE field framing, letting the harness inject
  additional SSE fields/events into the byte stream the browser's
  `EventSource` parses.
- **Attack scenario:** A malicious/hijacked harness (see the session-hijack
  finding) emits an event whose `type` is e.g.
  `"tool_call\ndata: {\"fake\":true}\n\nevent: done"` — this splits into
  multiple SSE messages from the browser's perspective. Because
  `internal/web/static/app.js` only ever inserts event data via
  `textContent` (never `innerHTML`/`eval`) and only reacts to a fixed
  allowlist of event names, this doesn't achieve script injection today —
  the practical impact is event/framing spoofing: forcing a premature
  `done`/`eof`/`permission_request` dispatch (triggering the client's
  `reload()`) or corrupting `Last-Event-ID` resume semantics for the
  operator's browser. That's a real (if currently low-impact) integrity
  problem in a channel the operator trusts to reflect ground truth, and
  the impact ceiling rises if `app.js` is ever extended to trust `event:`
  names for anything more sensitive.
- **Confidence:** medium — requires attacker control of an event's `type`
  field, which in the current protocol is only reachable via a
  compromised/hijacked harness, not a bare network client; the *current*
  client-side impact is bounded by `app.js`'s consistent use of
  `textContent`.
- **Evidence:**
  ```go
  func writeSSEEvent(w http.ResponseWriter, ev store.Event) {
      fmt.Fprintf(w, "id: %s\n", ev.ID)
      fmt.Fprintf(w, "event: %s\n", ev.Type)   // ev.Type is harness-controlled, unsanitized
      ...
  }
  ```
- **Remediation:** Validate/sanitize `ev.Type` at the point it's persisted
  (`pump.go`'s `appendProto`/`append`) — reject or replace event types
  containing `\r`/`\n`, and/or restrict to a known charset (the fixed set
  of harness event-type constants already enumerated in
  `controlplane.go:28-38` covers the legitimate cases; unknown types are
  already logged as unusual at `pump.go:130`, so a stricter charset check
  there costs nothing functionally).
- **CWE:** CWE-93 (Improper Neutralization of CRLF Sequences), CWE-113
  (HTTP Response Splitting family / header injection analogue for SSE).

---

### [MEDIUM] Web UI forms have no CSRF protection, and no clickjacking headers — a bigger deal than "no auth" alone once an operator adds ingress auth

- **File:** `internal/web/handlers.go` (`submit`, `cancel`,
  `answerPermission` — all plain `POST` with no token), `internal/web/render.go`
  (no `X-Frame-Options`/`Content-Security-Policy`/`X-Content-Type-Options`
  set anywhere)
- **Description:** `POST /jobs`, `POST /jobs/{id}/cancel`, and
  `POST /jobs/{id}/permissions/{requestID}` accept ordinary
  `application/x-www-form-urlencoded` submissions with no CSRF token, no
  `SameSite` cookie dependency (there are no cookies at all today), and no
  `Origin`/`Sec-Fetch-Site` check. There are also no anti-clickjacking
  headers on any response.
- **Why this is worse than the documented posture, not just a restatement
  of it:** `docs/design.md` says "front it with your ingress's auth" — the
  most natural way an operator does that for a browser-driven UI is
  cookie/session-based SSO at the ingress (e.g. oauth2-proxy). The moment
  that's added, the *browser* carries ambient credentials to hairpin, and
  CSRF becomes exploitable exactly as normal: a page the operator's browser
  visits (or an `<iframe>`/auto-submitting `<form>` on it) can submit a job,
  cancel a running job, or **approve/deny a pending tool-permission
  request** on the operator's behalf, without their knowledge — the most
  sensitive action this UI exposes. The design doc's "front with auth"
  guidance, taken literally, does not by itself close this gap; it needs
  saying explicitly so the operator picks header-based/mTLS auth or adds
  CSRF tokens, not just cookie SSO.
- **Attack scenario:** Operator has ingress SSO (cookie-based) in front of
  hairpin per the documented guidance. Operator, in the same browser,
  visits an attacker-controlled page containing:
  `<form action="https://hairpin.internal/jobs/hp-.../permissions/req-1" method=POST><input name=allow value=true></form>` +
  auto-submit script. The operator's authenticated cookie rides along;
  hairpin has no way to tell this apart from a real click.
- **Confidence:** high that the gap exists; the exploitability is
  conditional on the operator's specific choice of "front it with auth"
  (cookie-based vs. header/mTLS-based) — worth flagging now while it's
  cheap, rather than after an operator's chosen auth model turns this
  latent gap into a live one.
- **Remediation:** Add a `SameSite=Strict` requirement note for any
  cookie-based fronting auth in the README security section (see posture
  notes below), and/or add double-submit CSRF tokens to the three POST
  forms regardless of auth model (cheap, and correct defense-in-depth even
  under bearer-token/mTLS fronting where CSRF wouldn't otherwise apply).
  Add `X-Frame-Options: DENY` and a baseline `Content-Security-Policy`
  (`default-src 'self'`) to `render` and the SSE handler.
- **CWE:** CWE-352 (Cross-Site Request Forgery), CWE-1021 (Improper
  Restriction of Rendered UI Layers, clickjacking).

---

### [LOW] Job/request IDs are not format-validated at the service/API layer — Redis key-namespace confusion between job/events/perms

- **File:** `internal/service/service.go` (`Get`, `Cancel`, `AnswerPermission`
  pass `id`/`jobID`/`requestID` straight through), `internal/api/api.go`
  (no validation before calling into `service`), `internal/store/redisstore/redisstore.go:53-63`
  (`jobKey`/`eventsKey`/`permsKey` are simple string concatenation)
- **Description:** The web UI validates job IDs against
  `^hp-[0-9a-z]{26}$` (`internal/web/web.go:43-45`) before calling into
  `service`, but the connect `JobService` API (`internal/api/api.go`) does
  not — `req.Msg.GetId()` goes directly to `s.svc.Get/Cancel/...` and from
  there straight into Redis key construction. Because
  `eventsKey(id) = jobKey(id) + ":events"` and `permsKey(id) = jobKey(id) +
  ":perms"`, a caller supplying `id = "<realJobID>:perms"` makes
  `jobKey("<realJobID>:perms")` collide exactly with
  `permsKey("<realJobID>")`. `GetJob` would then run `HGetAll` against the
  *real* job's permissions hash (both are Redis hash types, so no
  `WRONGTYPE` error) and attempt to parse its fields as job fields — bounded
  in practice because `jobFromFields` only reads specific field names
  (`id`, `status`, `prompt`, ...) and permission-hash field names are
  `request_id` values, so a collision that leaks anything meaningful
  requires a `permission_request`'s `request_id` to *also* happen to equal
  one of those field names (itself only reachable via harness control,
  which already implies the much higher-impact session-hijack finding
  above). The `:events` collision is not exploitable the same way — the
  events key is a Redis stream, not a hash, so `HGetAll` on it fails
  outright.
- **Attack scenario:** Low standalone impact today given the above
  constraints, but this is a fragile invariant: any future field added to
  a permission/event record whose name happens to coincide with a job
  field, or any future store method that does something less
  type-safe than `HGetAll`, turns this into real cross-object data
  exposure. It's also simply inconsistent that the same validation exists
  in one caller (web UI) and not the other (API), for no principled
  reason.
- **Confidence:** medium — verified the key-construction collision is real;
  did not find a currently-exploitable data leak beyond the narrow overlap
  described.
- **Remediation:** Validate job ID (and permission `requestID`) shape once,
  in `internal/service`, shared by both the web and API front ends, rather
  than duplicating (or omitting) the check per caller. Reuse
  `job.NewID`'s format as the source of truth for the regex instead of
  hand-duplicating it in `internal/web/web.go`.
- **CWE:** CWE-20 (Improper Input Validation), CWE-668 (Exposure of
  Resource to Wrong Sphere) as a latent risk.

---

### [LOW] Harness-launcher RBAC grants `get`/`list`/`watch`/`delete` on Jobs that the launcher code never uses

- **File:** `examples/k8s/rbac.yaml:16-25`, `internal/launcher/k8s.go` (only
  calls `Jobs(...).Create`)
- **Description:** The reference `Role` grants `create, get, list, watch,
  delete` on `batch/v1` `Jobs` in the namespace. `internal/launcher/k8s.go`'s
  `Launch` only ever calls `Create` (with `AlreadyExists` treated as
  success). `get`/`list`/`watch`/`delete` are unused by any code in this
  repo.
- **Attack scenario:** If the `hairpin` ServiceAccount's token is ever
  exfiltrated (e.g. via the RCE-adjacent finding above, or a future bug),
  the attacker inherits `delete` rights over *every* `batch/v1` Job in the
  namespace — not just ones hairpin created — including any unrelated
  workloads sharing that namespace. This is a straightforward
  least-privilege violation independent of any other finding.
- **Confidence:** high (verified against the only caller of the k8s client
  in the repo).
- **Remediation:** Drop the Role to `create` only (`get`/`watch` only if a
  future readiness-polling feature needs it — add them back when that code
  exists, not preemptively). If per-Pod log access for `kubectl logs`
  debugging is wanted, grant `pods/log: get` explicitly (as stirrup's own
  reference Role does deliberately, per its k8s executor docs) rather than
  broad Job `get/list/watch/delete`.
- **CWE:** CWE-269 (Improper Privilege Management).

---

### [LOW] No `NetworkPolicy` in the reference manifests

- **File:** `examples/k8s/` (no `networkpolicy.yaml`)
- **Description:** The reference manifests include a namespace, RBAC,
  Redis, hairpin's own Deployment, and a secret — no `NetworkPolicy`
  constrains which pods in the `hairpin` namespace (or the wider cluster,
  if the namespace has no isolation) can reach hairpin's control-plane
  port, Redis, or vice versa. Given the entire premise of this review's
  first two findings is "anything on the network can reach the control
  plane," namespace-level network isolation is the most direct
  cluster-native mitigation available today, short of adding real
  authentication.
- **Confidence:** high (absence verified by directory listing).
- **Remediation:** Add a default-deny ingress `NetworkPolicy` for the
  `hairpin` namespace, with explicit allows for: harness Pods → hairpin's
  h2c port (control plane), hairpin → Redis, and (if the UI/API needs
  external reach) ingress-controller → hairpin. This is good practice
  regardless of the auth-posture findings above and costs nothing beyond
  documentation.
- **CWE:** CWE-284 (Improper Access Control) / defense-in-depth gap.

---

## Posture notes (acceptable as documented — for the README's security section, not action items)

These are consequences of the deliberate v1 trust posture in
`docs/design.md` and are not new findings — listed so they land in the
README's security section alongside the operational guidance that already
exists in `docs/design.md` and `examples/k8s/README.md`.

- **Plaintext, unauthenticated gRPC control plane and JobService/UI.**
  Documented in `docs/design.md` ("Trust posture (v0.1)") and
  `examples/k8s/README.md` ("Trust posture"). Confirmed both `h2c`
  protocols are enabled deliberately (`cmd/hairpin/serve.go:125-135`) to
  support stirrup's plaintext HTTP/2-prior-knowledge dial. No Ingress is
  included in the reference manifests, correctly keeping the Service
  cluster-internal by default.
- **`RunConfigJSON` (which may carry `dynamicContext`) is stored in Redis
  in plaintext.** This is necessary for the harness hand-off to work and
  is documented in `docs/design.md`'s Redis layout table. Worth an
  explicit README callout that anyone with Redis access (or a Redis
  backup/snapshot) sees this — not a bug, but worth operators knowing
  Redis needs the same trust boundary as hairpin itself.
- **No rate limiting anywhere** (SubmitJob, WatchJob/SSE connections,
  AnswerPermission). Consistent with "no auth, trusted network" — but
  becomes relevant the moment an operator exposes the API more broadly
  than intended, so it's worth a one-line README mention alongside the
  auth guidance.
- **Single-replica, in-process session registry**
  (`internal/registry/registry.go`) — already documented as a v1 scope
  cut in `docs/design.md`. Confirmed this is exactly what's implemented
  (a plain `map[string]Session` behind a mutex, no cross-replica
  coordination).
- **Multi-tenant `EnvFromSecrets` (one secret set for every harness Pod
  regardless of job).** Related to, but distinct from, the HIGH finding
  above about executor defaulting — even with `executor` correctly
  restricted to sandboxed types, every harness Pod's *orchestrator*
  process still sees every configured provider secret. This is a
  reasonable v1 simplification given the single-tenant assumption the
  rest of the trust posture already makes, but worth naming explicitly in
  the README so an operator running multiple unrelated provider
  credentials through one hairpin deployment understands the blast
  radius of any one compromised job.
- **`k8s-env-from-secrets` values, RBAC scope, and `--k8s-image` are
  fully operator-controlled flags/manifest edits**, not defaults baked
  into the code — the reference manifests correctly use a placeholder
  (`REPLACE_ME`) rather than a real key, and `examples/k8s/README.md`
  calls out what to edit before applying. No secrets are committed to the
  repo.

---

## Testing coverage observations

- `internal/controlplane/controlplane_test.go` has solid coverage of the
  *duplicate*-session case (`TestRunTaskDuplicateSession`) and unknown/
  terminal-job cases, but — reasonably, since it's how the protocol is
  designed to work today — no test exercises (or documents as a known gap)
  a stream claiming a *different, live* job's session ID. If the
  possession-proof fix above lands, a regression test asserting that a
  `ready.id` without the right token is rejected (distinct from "unknown
  job") would be the natural place to lock this in.
- No test exercises `SubmitJob`/`GetJob`/`CancelJob`/`AnswerPermission`
  with a malformed (non-`hp-<ulid>`) ID via the connect API to confirm
  current behavior at the Redis-key-collision boundary described in the
  LOW finding above — worth adding once/if that validation is centralized.
- No test covers oversized `run_config_json` or oversized `HarnessEvent`
  fields — expected, since no size bound exists yet to test against.

---

## Methodology / what was checked and found clean

- **SQL/command injection:** No SQL datastore. No `os/exec`,
  `exec.Command`, or shell-out beyond `internal/launcher/process.go`
  (dev/test-only launcher), which builds `exec.Command(p.cfg.StirrupBin,
  "job")` with a fixed argument list and structured `env []string` — no
  string interpolation of caller-controlled data into the command line or
  a shell. Job IDs used as directory-name suffixes
  (`os.MkdirTemp("", "hairpin-"+j.ID)`) are hairpin-generated ULIDs, not
  caller-controlled, so no path traversal there.
- **K8s launcher injection:** `internal/launcher/k8s.go` builds a
  structured `batchv1.Job` via the typed client-go API throughout — no
  string templating of a manifest, no caller-controlled data reaching a
  shell inside the created Pod's command/args (`Args: []string{"job"}` is
  fixed; only `CONTROL_PLANE_ADDR`/`CONTROL_PLANE_SESSION_ID` env vars are
  set, both hairpin-controlled). Job `Name: j.ID` is a hairpin-generated
  ULID (fixed charset/length), safe as a Kubernetes object name.
  `RunConfigJSON` is never used as an env var, arg, or label — it's only
  sent over the wire via `task_assignment`, so no injection surface there.
- **XSS in server-rendered templates:** All `internal/web/templates/*.html`
  use `html/template` (auto-escaping, contextual) exclusively —
  `text/template` is not imported anywhere in `internal/web`. Verified
  fields that carry externally-influenced content (`Prompt`, `FinalText`,
  `Error`, `InputJSON`, `StopReason`, permission `Reason`) are all rendered
  through `{{...}}` in HTML-body/attribute contexts that `html/template`
  correctly escapes.
- **XSS via the SSE→JS path:** `internal/web/static/app.js` only ever
  writes event data via `.textContent` (`appendLine`, `appendText`),
  never `.innerHTML`/`insertAdjacentHTML`/`eval`/`document.write`. Even
  fully attacker-controlled event payloads cannot execute script through
  this path today (see the CRLF/framing finding above for the one caveat
  found).
- **SSRF:** No outbound HTTP calls originate from hairpin itself based on
  caller input — `internal/web` makes no outbound requests, and
  `internal/launcher` only talks to the Kubernetes API server (fixed,
  operator-configured endpoint) or spawns a local process. RunConfig
  content that might reference external URLs (e.g. provider endpoints) is
  opaque to hairpin — it's forwarded to the harness, which is outside this
  review's code boundary.
- **CORS:** No CORS headers are set anywhere (`grep` found no
  `Access-Control-*` handling), meaning browsers enforce same-origin by
  default for the API/UI — not a gap, just confirmed absent rather than
  misconfigured permissively.
- **Secrets in logs:** No log call anywhere in `internal/` includes
  `RunConfigJSON`, `run_config_json`, or any field carrying it. Error logs
  around RunConfig parsing (`controlplane.go:212`,
  `launcher/k8s.go:156-157`) log the parse *error*, not the payload.
- **Secrets committed to the repo:** `examples/k8s/secret.yaml` uses a
  `REPLACE_ME` placeholder, not a real credential. No API keys, private
  keys, or tokens found hardcoded anywhere in `.go` files (checked via
  pattern search across `internal/`, `cmd/`, excluding test files and
  legitimate matches like `EnvFromSecrets`/`SecretRef`).
- **JWT / crypto:** No JWT handling, no custom cryptography, no RNG use
  beyond `job.NewID`'s `crypto/rand`-backed ULID generation (correct
  choice — not `math/rand`).
- **Race conditions / TOCTOU:** `redisstore.UpdateJob` uses Redis
  `WATCH`/`MULTI` optimistic concurrency control with bounded retries
  (`jobs.go:198-243`) — reviewed for correctness, no obvious lost-update
  window. `registry.Unregister` correctly checks identity
  (`cur == s`) before deleting, preventing a slow-exiting stream from
  evicting its replacement (`registry.go:53-59`) — this is the *right*
  pattern; the gap is upstream of it (no auth on *who* gets to register in
  the first place, per the HIGH finding).

No zero-finding categories beyond the above were left unexamined; every
category in the review brief has at least one explicit finding or an
explicit "checked, clean" note above.
