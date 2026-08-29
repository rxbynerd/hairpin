# Deployment

## Kubernetes

[`examples/k8s/`](../examples/k8s/) is a minimal, applyable starting
point: hairpin itself, the RBAC its launcher needs, a bare-bones
Redis, and a placeholder Secret for provider API keys.

| File | Kind | Purpose |
|---|---|---|
| `namespace.yaml` | Namespace | The `hairpin` namespace everything below lives in. |
| `rbac.yaml` | ServiceAccount + Role + RoleBinding | The `hairpin` identity and the `batch/v1` Jobs verbs the launcher needs today: create, get. |
| `redis.yaml` | Deployment + Service | Single-replica, unpersisted Redis for `internal/store/redisstore`. Fine for a kind cluster; swap for a managed instance otherwise — see [Redis](#redis). |
| `hairpin.yaml` | Deployment + Service | hairpin itself, flags wired to the k8s launcher and the Redis above. |
| `secret.yaml` | Secret | Placeholder provider API keys, exposed to harness Pods via `-k8s-env-from-secrets`. Replace the value before applying, or generate the Secret out-of-band and drop this file. |

### What to edit before applying

- `hairpin.yaml`: `containers[0].image` (a built-and-pushed hairpin
  image) and `-k8s-image` (the stirrup image the launcher runs as
  harness Jobs).
- `secret.yaml`: the placeholder `ANTHROPIC_API_KEY` value, or any
  other provider keys your RunConfig profiles reference.

### Apply order

```sh
kubectl apply -f examples/k8s/namespace.yaml
kubectl apply -f examples/k8s/rbac.yaml
kubectl apply -f examples/k8s/redis.yaml
kubectl apply -f examples/k8s/secret.yaml
kubectl apply -f examples/k8s/hairpin.yaml
```

or, once the images and Secret are edited, `kubectl apply -f
examples/k8s/` applies all five in one pass — `kubectl apply` is
order-independent within a single invocation.

Submitting a job once the `hairpin` Service is up: port-forward or
call `JobService/SubmitJob` from another Pod in the cluster (see
[`docs/api.md`](api.md)).

### The advertise address and the Service

`-advertise` is the value placed in `CONTROL_PLANE_ADDR` for every
harness hairpin launches — it must be the address a Pod in the
harness's namespace can dial back into hairpin on, not the address a
caller submitting jobs uses. In `hairpin.yaml` that is the in-cluster
DNS name of hairpin's own Service:

```
--advertise=hairpin.hairpin.svc:8130
```

`<service>.<namespace>.svc` resolves cluster-wide, so this holds
whether harness Jobs land in the same namespace as hairpin or (via
`-k8s-namespace`) a different one, as long as both are on the same
cluster's DNS. `-listen` and `-advertise` are independent: `-listen`
is where hairpin binds, `-advertise` is what it tells harnesses to
dial — the manifest keeps both at `:8130`/`hairpin.hairpin.svc:8130`
because the Service forwards `port: 8130` to the same `containerPort`.

### Secret handling for provider keys

RunConfig fields that carry credentials (`provider.api_key_ref`,
VCS tokens, MCP headers, ...) accept a `secret://NAME` reference
instead of a plaintext value — the harness resolves it from its own
environment at run start, and `RunConfig.Redact()` scrubs the
reference (not the resolved value) before any trace or log write.
`-k8s-env-from-secrets` is how those names get into the harness Pod's
environment: a comma-separated list of Kubernetes Secret names,
mounted via `envFrom` on the `stirrup` container
(`internal/launcher/k8s.go`). `secret.yaml` creates one such Secret
(`provider-api-keys`, key `ANTHROPIC_API_KEY`) and `hairpin.yaml`
wires it in:

```
--k8s-env-from-secrets=provider-api-keys
```

A profile referencing `secret://ANTHROPIC_API_KEY` then resolves
against that environment variable inside the harness Pod. Add more
Secret names (comma-separated) for additional providers, VCS tokens,
or MCP credentials; hairpin itself never reads or handles the secret
values — it only tells Kubernetes which Secrets to mount.

### `activeDeadlineSeconds` and the deadline slack

The k8s launcher sets each harness Job's `activeDeadlineSeconds` to
the submitted RunConfig's `timeout` (the harness's own wall-clock
budget) plus `-k8s-deadline-slack` (default `10m`). The slack covers
everything the RunConfig timeout does not: the harness has up to 5
minutes to dial back and receive `task_assignment` before it gives up,
plus Pod scheduling, image pull, and process startup on the way in,
and git/trace finalisation on the way out. Without slack, a Job could
be killed by Kubernetes mid-finalisation even though the harness
itself respected its timeout. If a job's stored RunConfig has no
readable `timeout` (should not happen — `SubmitJob` requires one), the
launcher falls back to a 3600s baseline plus the same slack rather
than leaving the Job unbounded.

## Redis

`redis.yaml` is deliberately minimal: no persistence, no auth, no HA —
adequate for a kind cluster or a quick evaluation, not for anything
you want to survive a restart. Point `-redis` at a managed
Redis/Valkey instance, or a Redis with AOF or RDB persistence enabled,
for anything longer-lived.

**Persistence is recommended** for any deployment where job history
matters beyond the current Redis process's lifetime. Without it, a
Redis restart loses:

- Every job record (`hairpin:job:<id>`) — status, prompt, stored
  RunConfig, final text, timestamps. `GetJob`/`ListJobs` for jobs
  submitted before the restart return `not_found`.
- Every recorded event timeline (`hairpin:job:<id>:events`) — the
  history `WatchJob` and the web UI's SSE feed replay.
  In-flight harnesses are unaffected (they keep streaming to whichever
  hairpin process holds the live registry entry), but nothing gets
  persisted until Redis is back, and a hairpin restart during the
  outage loses the in-process registry too — see
  [Single-replica constraint](#single-replica-constraint).
- Pending permission requests (`hairpin:job:<id>:perms`) — an
  in-flight `ask-upstream` approval a caller has not yet answered.

Choosing no persistence is a legitimate call for ephemeral or CI-style
usage where nobody needs a job's history after it finishes; it is not
appropriate for a deployment callers rely on to look up past jobs.

## Operational notes

### Single-replica constraint

Run exactly one hairpin replica. The bridge from `AnswerPermission` /
`CancelJob` onto a live harness stream (`internal/registry`) is an
in-process map from job ID to open `RunTask` stream — it is not
shared across processes. A harness dials back to whichever hairpin
instance a load balancer happens to route it to; if a second replica
receives an `AnswerPermission` call for that job, it has no session to
route the decision onto and the call fails with `failed_precondition`.
`hairpin.yaml`'s Deployment is pinned to `replicas: 1` for this
reason. Scaling out requires moving the control-event bridge to Redis
pub/sub or an equivalent shared mechanism — tracked as a deferred v1
scope cut in [`docs/design.md`](design.md#deliberately-deferred-v1-scope-cuts).

### Heartbeat and staleness

`Job.last_event_at` updates on every event the harness sends,
including the heartbeat it emits roughly every 30 seconds while
running (`internal/controlplane/pump.go` flushes this at a bounded
rate, not on every event, so treat it as accurate to within a few
seconds rather than exact). A `running` job whose `last_event_at` has
not advanced for more than ~30-60 seconds is a hung or evicted
harness worth investigating — poll `GetJob` or watch for `heartbeat`
events on the timeline to observe this directly; hairpin does not
currently reap stale jobs on its own (see below).

### Job retention

Hairpin does not currently expire or delete job records, event
timelines, or permission requests on any schedule — there is no
job-level TTL or reaping job. The only built-in bound is that each
job's event stream is capped at roughly 10,000 entries
(`internal/store/redisstore.DefaultMaxEvents`, approximate `XADD
MAXLEN ~` trimming): oldest events are dropped once a single job's
timeline grows past that, but the job record itself, and every other
job's data, is kept indefinitely. Operators who need bounded storage
should manage this externally for now — a periodic sweep deleting
`hairpin:job:*` keys older than a retention window, or a Redis
`maxmemory`/eviction policy appropriate for a key space that never
expires on its own.

This absence is also why `rbac.yaml` grants only `create` and `get` on
`batch/v1` Jobs: the launcher doesn't list, watch, or delete Jobs
today, so the Role doesn't carry those verbs. A future reaper — for
Redis job records or for finished-but-not-yet-`ttlSecondsAfterFinished`
Kubernetes Jobs — needs `list`/`watch`/`delete` added back to that
Role before it can run.
