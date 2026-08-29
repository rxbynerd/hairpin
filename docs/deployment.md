# Deployment

## Kubernetes

Hairpin is a Kubernetes application in both directions: it runs inside
the cluster whose API it drives, launches each run as a `batch/v1` Job
from the published stirrup harness image, and that harness in turn
creates one sandbox Pod per run — the container an agent's shell
commands actually execute in — from the published stirrup sandbox
image.

That gives three identities, each holding the least it can:

| Identity | Is | Holds |
|---|---|---|
| `hairpin` | The server | `create`/`get` on `batch/v1` Jobs in its own namespace. |
| `stirrup-harness` | The harness Job hairpin launches | Pods, `pods/exec`, and NetworkPolicies in the *sandbox* namespace only. Its token is mounted; the Kubernetes executor authenticates as it. |
| `stirrup-sandbox` | The sandbox Pod the harness creates | Nothing. Its token is never mounted — it runs untrusted agent commands and has no business reaching the API server. |

[`examples/k8s/`](../examples/k8s/) is a minimal, applyable starting
point wiring all three together.

| File | Kind | Purpose |
|---|---|---|
| `namespace.yaml` | Namespace ×2 | `hairpin` (server, Redis, harness Jobs) and `hairpin-sandboxes` (the Pods agent commands run in). |
| `rbac.yaml` | ServiceAccount + Role + RoleBinding | The `hairpin` identity and the `batch/v1` Jobs verbs the launcher needs today: create, get. |
| `rbac-sandbox.yaml` | ServiceAccount ×2 + Role + RoleBinding | The `stirrup-harness` identity, its sandbox-namespace Role, and the token-less `stirrup-sandbox` identity the sandbox Pods run as. |
| `redis.yaml` | Deployment + Service | Single-replica, unpersisted Redis for `internal/store/redisstore`. Fine for a kind cluster; swap for a managed instance otherwise — see [Redis](#redis). |
| `profiles.yaml` | ConfigMap | The RunConfig profile templates mounted at `--profiles`. |
| `hairpin.yaml` | Deployment + Service | hairpin itself, wired to the identities, Redis, and profiles above. |
| `secret.yaml` | Secret | Placeholder provider API keys, exposed to harness Pods via `-harness-secrets`. Replace the value before applying, or generate the Secret out-of-band and drop this file. |

### What to edit before applying

- `hairpin.yaml`: `containers[0].image` — build and push a hairpin
  image, then point this at it. The stirrup images are already
  defaulted to their published tags; override with `-harness-image` and
  `-sandbox-image` to pin a digest or use a mirror.
- `profiles.yaml`: the shipped `default` profile names a model and an
  `ask-upstream` permission policy. Adjust it, or add profiles, for
  what your callers actually submit.
- `secret.yaml`: the placeholder `ANTHROPIC_API_KEY` value, or any
  other provider keys your RunConfig profiles reference.

Namespace and advertise address are not among them: an in-cluster
hairpin reads its namespace from its projected ServiceAccount and
advertises `hairpin.<namespace>.svc` on its listen port.

### Apply order

```sh
kubectl apply -f examples/k8s/
```

`kubectl apply` is order-independent within a single invocation, so
one pass over the directory is enough once the images and Secret are
edited.

Submitting a job once the `hairpin` Service is up: port-forward or
call `JobService/SubmitJob` from another Pod in the cluster (see
[`docs/api.md`](api.md)).

### The development cluster

[`scripts/dev/`](../scripts/dev/) brings up a single-node kind cluster
on podman and exercises the full chain against it:

```sh
just kind-up      # scripts/dev/kind-up.sh
just deploy       # build, load into the node, apply examples/k8s + a fake provider
just smoke-test   # submit one job, assert it completed in a sandbox Pod
just kind-down
```

`deploy.sh` overrides the reference Deployment's image with a locally
built `localhost/hairpin:dev` and installs
`scripts/dev/fake-provider.yaml` — a stand-in that speaks just enough
of the OpenAI chat-completions SSE protocol to drive one `run_command`
call and then finish, so a run completes with no API key and no egress
from the cluster. It also replaces the `hairpin-profiles` ConfigMap so
the default profile points at it. Development only.

For a real-model run, `just openrouter <op-ref>` /
[`scripts/dev/openrouter.sh`](../scripts/dev/openrouter.sh) reads an
OpenRouter API key from 1Password (`op-ref` is a secret reference like
`op://<vault>/<item>/credential`), merges it into the
`provider-api-keys` Secret, adds an `openrouter` profile, and restarts
hairpin — profiles are loaded once at startup. Submit with
`{"profile": "openrouter", "prompt": "..."}`. `deploy.sh` recreates the
Secret and profiles ConfigMap without these additions, so re-run the
script after every deploy.

The kind cluster installs no gVisor RuntimeClass, so sandbox Pods run
under the cluster default runtime and the harness logs an isolation
warning. Every other path — Job creation, the control-plane dial-back,
sandbox Pod creation, `pods/exec`, NetworkPolicy install and teardown —
is exercised as it would be in production. Set `-sandbox-runtime
gvisor` against a cluster that has the RuntimeClass.

### Sandbox coordinates

The `-sandbox-*` flags do not configure hairpin's own behaviour: they
are the values hairpin writes into each submitted RunConfig's executor
at submit time, for the `k8s` and `k8s-sandbox` executor types, wherever
the profile left the field empty.

| Flag | RunConfig field |
|---|---|
| `-sandbox-image` | `executor.image` |
| `-sandbox-namespace` | `executor.k8sNamespace` |
| `-sandbox-service-account` | `executor.k8sServiceAccount` |
| `-sandbox-runtime` | `executor.runtime` |

A profile therefore names only the isolation it wants — the executor
type and a network mode — and stays portable across deployments; the
operator running hairpin decides where in the cluster that isolation
happens and under what identity. A profile that pins its own value
keeps it, which is how a single hairpin can route one tenant's runs to
a dedicated namespace or a stricter RuntimeClass.

`-sandbox-namespace` defaults to `-namespace`, but the reference
manifests deliberately separate them. Sandbox Pods run untrusted agent
output; keeping them in their own namespace is what lets a
NetworkPolicy or ResourceQuota target agent workloads without touching
the server, and what keeps the harness's RBAC scoped to a namespace
that contains nothing else.

### Harness RBAC and the mounted token

The harness is the orchestrator of stirrup's Kubernetes executor. For
every run it creates a sandbox Pod, installs a NetworkPolicy confining
that Pod's egress, drives file I/O and command execution through the
`pods/exec` subresource, and deletes both at end of run. It does all of
that as the ServiceAccount named by `-harness-service-account`, whose
token hairpin mounts into the harness Pod for exactly this reason —
a harness launched without a ServiceAccount gets no token, and any
Pod-backed executor fails closed at construction.

`rbac-sandbox.yaml` binds a Role in the *sandbox* namespace to a
subject in hairpin's namespace, so the harness reaches across and holds
nothing where it runs. Both NetworkPolicy verbs are required: the
policy is installed before the Pod and removed with it, and a Pod whose
egress cannot be confined must never be created.

### The advertise address and the Service

`-advertise` is the value placed in `CONTROL_PLANE_ADDR` for every
harness hairpin launches — it must be the address a Pod in the
harness's namespace can dial back into hairpin on, not the address a
caller submitting jobs uses. That is the in-cluster DNS name of hairpin's own Service, which is
what `-advertise` defaults to in-cluster:

```
hairpin.<namespace>.svc:8130
```

`<service>.<namespace>.svc` resolves cluster-wide, so this holds
whether harness Jobs land in the same namespace as hairpin or (via
`-namespace`) a different one, as long as both are on the same
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
`-harness-secrets` is how those names get into the harness Pod's
environment: a comma-separated list of Kubernetes Secret names,
mounted via `envFrom` on the `stirrup` container
(`internal/launcher/k8s.go`). `secret.yaml` creates one such Secret
(`provider-api-keys`, key `ANTHROPIC_API_KEY`) and `hairpin.yaml`
wires it in:

```
--harness-secrets=provider-api-keys
```

A profile referencing `secret://ANTHROPIC_API_KEY` then resolves
against that environment variable inside the harness Pod. Add more
Secret names (comma-separated) for additional providers, VCS tokens,
or MCP credentials; hairpin itself never reads or handles the secret
values — it only tells Kubernetes which Secrets to mount.

### `activeDeadlineSeconds` and the deadline slack

The launcher sets each harness Job's `activeDeadlineSeconds` to the
submitted RunConfig's `timeout` (the harness's own wall-clock budget)
plus `-deadline-slack` (default `10m`). The slack covers
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
