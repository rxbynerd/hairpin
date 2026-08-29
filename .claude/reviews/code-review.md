# Hairpin code review

Scope: all code since `0dd6840` (everything under `internal/`, `cmd/`), per
`docs/design.md`. Excludes `gen/` and the vendored
`proto/harness/v1/harness.proto` from review, though the latter's doc
comments were read as the conformance oracle for finding #1.

`go test ./...` / `go vet ./...` were **not run** — no Bash tool was available
in this session. All findings below are from static reading of the source
and tests. Recommend running both before merging on the strength of this
review alone.

---

## CRITICAL

### 1. `done.stop_reason` success detection checks the wrong value — every successful run is likely recorded as `failed`

**Where:** `internal/job/job.go:83-92` (`StatusForStopReason`), consumed by
`internal/controlplane/pump.go:219-227` (`finish`).

```go
func StatusForStopReason(stopReason string) Status {
	switch stopReason {
	case "success":
		return StatusSucceeded
	case "cancelled":
		return StatusCancelled
	default:
		return StatusFailed
	}
}
```

The vendored `proto/harness/v1/harness.proto` documents the valid
`HarnessEvent.stop_reason` values twice (lines 75-77 and 156-160):

> `"end_turn", "max_turns", "timeout", "stalled", "tool_failures", "cancelled", "budget_exceeded", "error", "setup_failed", "hook_failed"`

`"success"` is not in that list. `"end_turn"` is the value that reads as
"the loop finished a turn on its own" — i.e. the happy path. The actual
`"success"` value lives on a *different, nested* field:
`HarnessEvent.trace.outcome` (`RunTrace.outcome`, proto lines 941-950),
which hairpin never reads (`grep -r "GetTrace\|Outcome" internal/` returns
nothing).

Consequence: if real `stirrup job` harnesses emit `stop_reason` values per
their own proto's doc comment, every successful run's `done` event carries
`stop_reason: "end_turn"`, which falls through `StatusForStopReason`'s
`default` case and is recorded as `job.StatusFailed`. Callers polling
`GetJob`/`WatchJob` and the web UI would see every successful task marked
failed.

This isn't a one-off typo — it's baked in consistently across the codebase:
`docs/design.md:49-52`, `internal/controlplane/controlplane_test.go`
(every happy-path test uses `StopReason: "success"`), and even hairpin's
own `docs/api.md` is internally self-contradictory: line 199 says
`JOB_STATUS_SUCCEEDED` means `stop_reason` was `"success"`, while lines
203-206 of the *same file* list the valid `stop_reason` values as
`end_turn, max_turns, timeout, stalled, tool_failures, cancelled,
budget_exceeded, error, setup_failed, hook_failed` — a list that includes
`end_turn` and excludes `success`. The contradiction is visible without
even opening the proto.

**Confidence:** high that the code/docs disagree with the vendored proto's
documented contract; medium-high that this manifests as a real bug in
production, contingent on the actual (unreviewed) stirrup harness
implementation matching its own proto doc comments. Given the proto is
described as the authoritative wire contract, I'd treat this as
needing verification against a real harness before shipping, at minimum.

**Suggested fix:** Map `"end_turn"` (not `"success"`) to `StatusSucceeded`
in `StatusForStopReason`, or — better, per the proto's own guidance that
`trace.outcome` is "the authoritative field for downstream analytics" —
read `ev.GetTrace().GetOutcome()` for the terminal status and keep
`stop_reason` purely as the verbatim display string. This also fixes
finding #7 below (discarded trace metrics) in one pass.

---

## HIGH

### 2. Graceful shutdown does not drain in-flight control-plane streams or SSE handlers; the process exits under them

**Where:** `cmd/hairpin/serve.go:137-160`.

```go
shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
if err := server.Shutdown(shutdownCtx); err != nil {
    logger.Warn("forced shutdown", "error", err)
}
svc.WaitForLaunches()
return nil
```

`http.Server.Shutdown` only force-closes *idle* connections; it polls for
active connections (including long-lived HTTP/2 streams — the
`RunTask` bidi stream and SSE `/jobs/{id}/events`) to go idle and, if the
passed context expires first, simply returns the context error while those
connections and their handler goroutines keep running in the background.

Because a `RunTask` stream is "active" for the entire life of a harness run
(up to `RunConfig.timeout`, validated up to 3600s in
`internal/service/submit.go:112-114`), essentially any real deployment with
a running job will hit the 10s deadline on every `SIGTERM`. The code then
only calls `svc.WaitForLaunches()` — which waits on `Service.launches`, the
*submit-time launcher* goroutines only (`internal/service/submit.go:59-60`),
not on control-plane sessions or SSE handlers — and returns. `serve()`
returning unwinds to `main()`, which exits the process immediately,
regardless of what other goroutines are doing (Go does not wait for
background goroutines on process exit).

**Concrete failure scenario:** operator does a routine rolling
deploy/restart while jobs are running. Every in-flight harness connection
is severed without hairpin sending a `cancel` control event, without a
`closeUnfinished`/`finish` settlement ever running (the pump goroutine is
killed mid-flight along with the process), and without flushing the last
buffered `text_delta` coalescing window. The affected jobs are left
permanently stuck in `running` in Redis (no terminal transition ever
happens for them), and any events buffered in `eventPump.deltas`/`finalText`
at the moment of the kill are lost. If the harness pod itself survives (it's
a separate Kubernetes Job) and retries its connection to a *new* hairpin
instance, correlation may or may not behave sanely — that's stirrup-side
and out of scope here, but from hairpin's side the abandoned job record is
a real, reproducible gap.

**Confidence:** high — this follows directly from `net/http`'s documented
`Shutdown` semantics and the absence of any other wait/drain mechanism for
control-plane or SSE goroutines.

**Suggested fix:** Track in-flight `RunTask` sessions (the registry already
enumerates them) and SSE handlers with a `sync.WaitGroup` or count, send
`cancel` to every live session on shutdown signal, and wait (with a
deadline sized to realistic run durations, or unbounded with a documented
operational expectation) before closing the store. At minimum, document
that hairpin restarts abandon running jobs today, since that's a
significant operational fact for anyone running this in production.

---

## MEDIUM

### 3. `eventPump.finish` does not guard against an already-terminal job, unlike every sibling finalizer

**Where:** `internal/controlplane/pump.go:219-242`.

`finish` (called on a `done` event) unconditionally overwrites
`j.Status`/`j.StopReason`/`j.FinalText`/`j.FinishedAt` via `UpdateJob`,
whereas `finaliseCancelled` (`controlplane.go:262-282`), `failJob`
(`controlplane.go:284-302`), and `closeUnfinished` (`pump.go:247-281`) all
check `j.Status.Terminal()` first and no-op (via the `errNoUpdate` sentinel)
if the job is already settled.

Tracing through the current call graph, I could not construct a live race
where `finish` runs concurrently with another finalizer while the same
session is registered — `Cancel()`'s "connected" branch only flips
`CancelRequested` and forwards a `cancel` control event, it never
transitions status directly, so today this looks safe by invariant rather
than by guard. But that invariant is easy to break in a future change (e.g.
a watchdog that force-finalizes stale sessions from outside the pump), and
the asymmetry with the other three finalizers — all of which *do* defend
against double-settlement — is a readability trap for the next person
touching this file.

**Confidence:** medium (real inconsistency; not a demonstrated live bug
today).

**Suggested fix:** Add the same `if j.Status.Terminal() { return errNoUpdate }`
guard to `finish` for defense in depth and consistency with its siblings.

### 4. `AnswerPermission` delivers the decision to the harness before persisting it

**Where:** `internal/service/service.go:164-196`.

```go
err = s.registry.Send(jobID, &harnessv1.ControlEvent{...})
...
p.State = store.PermissionAllowed // or Denied
...
if err := s.store.PutPermission(ctx, jobID, p); err != nil {
    return store.PermissionRequest{}, err
}
```

If `registry.Send` succeeds (the harness receives and acts on the
permission decision) but the subsequent `PutPermission` fails (Redis
hiccup, context deadline), the API call returns an error to the caller even
though the decision was already delivered, and the stored permission
request is left in `PermissionPending` state. A retrying or confused caller
can then call `AnswerPermission` again for the same `request_id` — since
the stored state still reads `pending` — potentially sending a second,
possibly conflicting, decision to a harness that has already acted on the
first one.

**Confidence:** medium (requires a store write failure in a narrow window;
consequence is state drift + a possible duplicate/conflicting answer, not
data loss).

**Suggested fix:** Persist the `Answered`/pending-consumed state (or at
least a "delivery attempted" marker) before or atomically with the send,
or treat a post-send persistence failure as fatal enough to at least log at
error level with enough context to reconcile manually (it already is
returned as an error, but nothing distinguishes "not delivered" from
"delivered but not recorded" for an on-call engineer to reconcile against
manually).

### 5. `memStore.AppendEvent` does a blocking channel send to slow watchers while holding the store's single global mutex

**Where:** `internal/store/memstore.go:116-142`, specifically:

```go
for _, w := range m.watchers[jobID] {
    select {
    case w.ch <- ev:
    case <-w.ctx.Done():
    }
}
```

`AppendEvent` holds `m.mu` (a single mutex shared by *every* job in the
store, not per-job) for its entire body. If a watcher's buffered channel
(`m.maxLen+64` capacity, so ~10k events) is full and its context is not
yet done, this send blocks — and since it's under `m.mu`, it blocks *every*
other `CreateJob`/`GetJob`/`UpdateJob`/`AppendEvent`/etc. call for *all*
jobs in the process, not just the slow watcher's job.

In practice this requires a watcher to fall roughly 10,000 events behind
without its context completing, which is unlikely in the dev/test contexts
`memStore` is intended for (per its doc comment: "in-memory Store for
tests and single-process dev runs"). Still, it's a real deadlock-shaped
bug reachable by a sufficiently slow SSE/Watch consumer, and integration
tests do exercise the real memstore-backed server (`internal/web/web_test.go`
uses a fake service, but `internal/integration` may use memstore — worth
checking if it's ever pointed at real load).

**Confidence:** medium — real code smell, low likelihood of triggering
given current call sites and buffer size, but a global lock plus a
blocking send is a footgun if `memStore` is ever reused somewhere less
controlled (e.g. as a load-test harness or a "just use in-memory for now"
production shortcut, which `cmd/hairpin/serve.go:163-166` explicitly makes
easy to do by omitting `-redis`).

**Suggested fix:** Send outside the lock (snapshot watcher channels under
the lock, then send after releasing it), or make the per-watcher channel
send non-blocking with a drop-and-log policy, matching how a slow SSE
client should be handled anyway.

### 6. SSE event framing does not sanitize the harness-controlled `Type` field

**Where:** `internal/web/sse.go:83-94` (`writeSSEEvent`), fed by
`store.Event.Type`, which for most event types is set directly from
`ev.GetType()` on the harness's wire message
(`internal/controlplane/pump.go:127-131`, the `default:` branch that
intentionally "tolerates unknown event types").

```go
fmt.Fprintf(w, "event: %s\n", ev.Type)
```

`HarnessEvent.type` is a free-form string field with no format constraint
enforced by hairpin (by design — see the "tolerating unknown event types"
requirement). If a (trusted-per-design-doc, but still non-hairpin-owned)
harness process ever sent a `type` containing a newline, that value would
terminate the `event:` line early and let the harness inject arbitrary
additional SSE fields (forged `id:`/`data:`/`event:` lines) into every
browser client watching that job's `/events` feed. Because `app.js` uses
`textContent` (not `innerHTML`) this does not appear to be exploitable as
XSS, but it can spoof event routing (e.g. forging a `done` event to trigger
the client's `reload()` handler, or corrupting `Last-Event-ID` resumption).

**Confidence:** low-medium — requires a malicious or badly-buggy harness on
what the design doc explicitly treats as a trusted network/component; not
exploitable by an external/API caller. Still a cheap, worthwhile hardening
given the doc comment on `HarnessEvent.type` explicitly says values are
unconstrained.

**Suggested fix:** Reject or strip control characters (`\r`, `\n`) from
`ev.Type`/`ev.ID` before writing SSE frames, same as you'd sanitize any
value going into a line-oriented text protocol.

### 7. `RunTrace` (tokens, cost, turns, duration, `outcome`) from the `done` event is discarded entirely

**Where:** `internal/controlplane/pump.go:219-242` (`finish`) never reads
`ev.GetTrace()`.

Not mentioned in `docs/design.md`'s "Deliberately deferred" list, so this
looks like an oversight rather than an intentional v1 cut. The `Job` model
(`internal/job/job.go`) has no fields for cost/tokens/turns, and none of
`internal/api/convert.go`'s `jobToProto` surfaces them, so this data is
unrecoverable after the fact. This is also the fix path for finding #1
(`trace.outcome` is explicitly documented as the authoritative
success/failure signal).

**Confidence:** high that the data is dropped; the "should we keep it" call
is a product decision, not just a bug, so filing as medium severity.

---

## LOW

### 8. Process launcher leaks per-job temp directories and log files

**Where:** `internal/launcher/process.go:36-91`.

`os.MkdirTemp("", "hairpin-"+j.ID)` is used when no `WorkDir` is configured,
and `harness.std{out,err}.log` files are opened per job; none of it is ever
removed after the harness process exits (`reap` only closes the file
handles, `process.go:82-91`). Explicitly a dev/e2e-only launcher per its
doc comment, and "hairpin never kills the harness" is a stated design
choice, but unbounded temp-dir accumulation on a long-running dev box is
still worth a one-line cleanup or a note in the doc comment.

**Confidence:** high (directly observable), severity low given documented
dev-only scope.

### 9. `service.Watch`'s wrapper goroutine doesn't cancel the underlying `store.WatchEvents` context when it stops early

**Where:** `internal/service/watch.go:33-47` relative to
`internal/store/redisstore/events.go:104-141` (and the equivalent in
`memstore.go:166-201`, which is fine because it doesn't have this coupling
issue).

When the wrapper goroutine in `Watch` sees a terminal `status_change` event
and returns (closing `out`), it does not drain or cancel `src`
(`store.WatchEvents`'s channel). The underlying Redis polling goroutine
keeps running — bounded only by whatever context the *caller* originally
passed to `Watch` (e.g. an HTTP request context). In both current call
sites (`internal/web/sse.go:41`, `internal/api/api.go:74`) that context is
the request/stream context, which `net/http`/connect-go cancel promptly
once the top-level handler returns, so in practice this resolves within
~1 poll interval (150ms). But the design is fragile: any future caller of
`Service.Watch` with a longer-lived context (a background reconciliation
job, a test using `context.Background()`) would leak a polling goroutine
per call for the life of that context.

**Confidence:** medium — not a leak today given the two real call sites,
but a latent footgun given the layering.

**Suggested fix:** Have `Watch`'s wrapper goroutine own a derived,
cancellable context and cancel it on every exit path (terminal event,
`ctx.Done()`, natural channel close), rather than relying on the ultimate
caller's context lifetime.

### 10. `requestID` path segment isn't format-validated

**Where:** `internal/web/handlers.go:110-116` (`answerPermission`) only
checks `requestID == ""`, unlike `id` which is checked against
`validJobID`'s regex.

Not currently exploitable — `requestID` only ever reaches
`store.GetPermission`/`PutPermission` as a Redis hash *field* (parameterized
via go-redis, no string concatenation) and is HTML-escaped by
`html/template` wherever it's rendered — but it's an inconsistency worth
tidying for defense-in-depth and readability (every other path parameter in
this handler set is validated up front).

**Confidence:** high (observable), severity low (no demonstrated exploit
path).

### 11. `memStore.AppendEvent` computes `ev.ID` twice when `ev.At` is zero

**Where:** `internal/store/memstore.go:122-129`.

```go
m.seq++
ev.ID = fmt.Sprintf("%d-%d", ev.At.UnixMilli(), m.seq)
if ev.At.IsZero() {
    ev.At = time.Now()
    ev.ID = fmt.Sprintf("%d-%d", ev.At.UnixMilli(), m.seq)
}
```

Functionally correct (the second assignment overwrites the first with the
real timestamp), just wasted work computing an ID from the zero-value time
(a large negative `UnixMilli`) that's immediately discarded. Minor
readability nit — reorder the zero-check first.

**Confidence:** high, trivial.

### 12. K8s Job pod spec has no `allowPrivilegeEscalation: false` / `readOnlyRootFilesystem`

**Where:** `internal/launcher/k8s.go:99-101`.

The launcher already sets `RunAsNonRoot: true` and disables
`AutomountServiceAccountToken`, which is good baseline hardening. It does
not set `AllowPrivilegeEscalation: false`, drop Linux capabilities, or set
`ReadOnlyRootFilesystem`. Given `stirrup job` needs to write to a workspace
directory, a fully read-only root FS may not be practical, but
`allowPrivilegeEscalation: false` and capability dropping are usually free.
Opinionated hardening suggestion, not a finding against a stated
requirement.

**Confidence:** high (observable), this is a suggestion rather than a
defect — labeled low severity accordingly.

---

## Confirmed non-issues

- **`registry.Unregister`'s identity check** (`internal/registry/registry.go:53-59`)
  correctly guards against a slow-exiting stream evicting its replacement
  by comparing the stored session pointer, not just the job ID. Correct as
  written.
- **`redisstore.UpdateJob`'s CAS retry loop** (`internal/store/redisstore/jobs.go:198-243`)
  correctly propagates `errNoUpdate`-style sentinel errors from the
  transaction callback without wrapping, so `errors.Is` checks in
  `internal/controlplane/controlplane.go` (`finaliseCancelled`, `failJob`)
  work as intended. Verified against `TestUpdateJobFnErrorNotPersisted` and
  `TestUpdateJobCASContention`.
- **`ListJobs` pagination** (`internal/store/redisstore/jobs.go:154-196`):
  using `ZADD` with a constant score of `0` plus `BYLEX` range queries is a
  legitimate and correct pattern for lex-ordering by a sortable ID (ULID);
  the `"(" + pageToken` exclusive-bound cursor with `Rev: true` correctly
  continues from strictly-older IDs. Verified against
  `TestListJobsPagination`.
- **`html/template` auto-escaping**: all web UI templates
  (`internal/web/templates/*.html`) rely on `html/template`'s contextual
  auto-escaping with no `template.HTML`/`template.JS` casts anywhere in
  `internal/web`. Harness- and caller-controlled strings (prompt, error,
  tool input, permission reason) are all escaped correctly, including
  inside the `action="..."` URL context. No XSS found in the server-rendered
  UI.
- **`static/app.js`**: uses `textContent`, never `innerHTML`, for all
  harness-sourced event data rendered into the DOM. No client-side
  injection found.
- **K8s launcher job naming**: `j.ID` (`"hp-" + lowercase ULID`) is used
  directly as the Kubernetes Job `Name`; this satisfies DNS-1123 label
  constraints and is generated server-side (`job.NewID`), so there's no
  injection or malformed-name risk from caller input.
- **`Registry.Register`/`session.Send` mutex discipline**
  (`internal/controlplane/controlplane.go:140-159`): correctly serializes
  all writers (event pump, permission-answer delivery via
  `registry.Send`) onto one `sync.Mutex`-guarded `Send`, matching the
  documented "connect's bidi Send is not safe for concurrent use"
  constraint. `TestSessionSendIsRoutedThroughRegistry` exercises this path.
- **`task_assignment` ordering / protocol conformance**: confirmed
  `runTask` sends `task_assignment` as the very first `ControlEvent` after
  registering the session (`controlplane.go:197-222`), before any other
  event type can be processed — matches the proto's "must be the first
  ControlEvent sent after the stream opens" requirement.
- **`OptionalBool` usage**: `permission_response.allowed`
  (`service.go:176`) and `sandbox_token_response.is_error`
  (`pump.go:194`) both correctly wrap values in `&harnessv1.OptionalBool{Value: ...}`
  rather than leaving proto3 scalar defaults ambiguous.
- **`sandbox_token_request` refusal** (`pump.go:190-201`): matches
  `docs/design.md`'s documented v1 scope cut (explicit `is_error` refusal
  so opted-in configs fail fast) and the proto's documented fail-closed
  60s-timeout behavior on the harness side.
- **Job ID validation on all web path parameters** except the permission
  `requestID` case (finding #10): `detail`, `cancel`, and `events` all
  check `validJobID` before touching the store.
