# Shared memory with Billet

Hairpin can give every run it launches two extra tools, `search_memory`
and `save_memory`, backed by [Billet](https://github.com/rxbynerd/billet),
the equestrianism suite's memory store. Knowledge one session saves is
searchable by the next, so runs launched by the same hairpin build on
one another's work.

The tools are **control-plane-fulfilled**: the harness never talks to
Billet. A call travels up the `RunTask` stream the harness already
holds, hairpin proxies it to Billet's `billet.v1.MemoryService` over
connect-go, and the result travels back down the same stream. The agent
environment gains no network path to the knowledge store, which keeps
stirrup's no-direct-egress posture intact and matches Billet's proxied
deployment model (`billet serve --rpc --mcp=false`).

```
harness ──tool_result_request{tool_name:"search_memory", input}──▶ hairpin
   ▲                                                                 │
   │                                              SearchMemory (gRPC/h2c)
   │                                                                 ▼
   └──tool_result_response{content, is_error}◀──────────────────── billet
```

Two upstream changes carry this feature, both merged:

- The `tools.controlPlane` RunConfig surface comes from
  [stirrup PR #586](https://github.com/rxbynerd/stirrup/pull/586).
  Hairpin's vendored `proto/harness/v1/harness.proto` matches stirrup
  main, and the published `ghcr.io/rxbynerd/stirrup:latest` image
  registers the tools.
- Billet's container image comes from
  [billet PR #1](https://github.com/rxbynerd/billet/pull/1), published
  as `ghcr.io/rxbynerd/billet:latest`, the image
  [`examples/k8s/billet.yaml`](../examples/k8s/billet.yaml) names.

## Configuration

| Flag | Default | Meaning |
|---|---|---|
| `-billet-addr` | *(empty)* | `host:port` of Billet's RPC listener, dialled with plaintext gRPC (HTTP/2 prior knowledge). Both halves are required and the port must be a number from 1 to 65535; `:8141` or a bare hostname fails at startup. Empty disables memory: submits that declare the memory tools are rejected. |

One flag decides both halves of the feature: whether `SubmitJob` accepts
a RunConfig that declares the memory tools, and whether the control
plane answers calls to them. Nothing is dialled until the first call,
so hairpin starts whether or not Billet is up; a Billet that is down
surfaces as a tool failure on each call.

Billet binds its memory namespace once at startup; every job hairpin
proxies shares that namespace. That is the point, but it also makes a
hairpin deployment one memory scope with the consequences described
under [Trust posture](#trust-posture). Run separate Billet instances
behind separate hairpins to partition.

## Declaring the tools in a profile

A profile opts in by declaring the two tools as control-plane tools in
its RunConfig. Stirrup registers each entry as an async tool whose
result the control plane supplies:

```json
"tools": {
  "builtIn": ["read_file", "run_command"],
  "controlPlane": [
    {
      "name": "search_memory",
      "description": "Search knowledge saved by earlier sessions. Call this before starting work on a task.",
      "inputSchema": {
        "type": "object",
        "properties": {
          "query": {"type": "string"},
          "limit": {"type": "integer", "description": "Maximum records to return (default 5, max 100)."}
        },
        "required": ["query"]
      }
    },
    {
      "name": "save_memory",
      "description": "Save a fact or outcome that a future session would benefit from knowing.",
      "inputSchema": {
        "type": "object",
        "properties": {
          "content": {"type": "string"},
          "kind": {"type": "string", "enum": ["event", "fact"]}
        },
        "required": ["content"]
      }
    }
  ]
}
```

The reference `default` profile in
[`examples/k8s/profiles.yaml`](../examples/k8s/profiles.yaml) and the
development fake-provider profile both carry this declaration.

Hairpin matches on the tool **name**: `search_memory` and `save_memory`
are the only control-plane tools it fulfils. The `description` text is
what steers the model; a profile that wants sessions to share knowledge
should say so there, or in `systemPromptOverride`. Name syntax, schema
shape, and `requiresApproval` are stirrup's to validate and enforce;
hairpin adds only the checks below.

### Submit-time checks

`SubmitJob` rejects a RunConfig with `invalid_argument`, before a job is
created, when its `tools.controlPlane` list:

- names any tool other than `search_memory` or `save_memory`. Hairpin
  could never answer it, and the harness would block for the per-call
  timeout on every use;
- names a memory tool while hairpin runs without `-billet-addr`;
- sets a non-zero `timeoutSeconds` shorter than hairpin's 10 second
  Billet call timeout. A harness that gave up sooner would report a
  failure for a save Billet still commits, inviting a retry and a
  duplicate record. Zero keeps stirrup's 60 second default.

### The declaration is checked at run time too

When a harness claims its job, the control plane reads the set of
control-plane tool names from the job's stored RunConfig. A
`tool_result_request` naming a tool outside that set is refused, even
when the tool is one hairpin could fulfil, so a profile that omits the
memory tools is genuinely opted out of them and a harness cannot reach
memory by naming a tool its run never declared.

## Tool contracts

Inputs and outputs mirror Billet's own MCP tool surface, so a model sees
the same contract whether it reaches Billet directly or through hairpin.

| Tool | Input | Output (`content`) |
|---|---|---|
| `search_memory` | `{"query": string, "limit"?: int}` | `{"records": [{"memory_id", "content", "score", "created_at"}]}` |
| `save_memory` | `{"content": string, "kind"?: "event" \| "fact"}` | `{"memory_id": string, "accepted": bool}` |

Hairpin applies Billet's request limits itself so an oversized call is
named as the caller's mistake rather than surfacing Billet's transport
error: `limit` is clamped to 100, with zero or below left for Billet
to default (5), and a `content` over 256 KiB is refused as malformed
input. `search_memory` results are best matches first; Billet applies
no score threshold, so a query that matches nothing still returns
records. `accepted: true` means Billet took the save, not that it is
searchable yet.

### Errors and refusals

Errors reach the model as a tool failure (`is_error: true`):

- Malformed input (not a JSON object, a field of the wrong type, a
  missing or blank `query`/`content`, an unknown `kind`, an oversized
  `content`): the message names the tool and the field.
- Billet rejected the call (`INVALID_ARGUMENT`, or
  `RESOURCE_EXHAUSTED` when its budget is spent): Billet's message,
  verbatim.
- Billet unreachable or any other failure, including the 10 second
  timeout: the fixed message `memory is unavailable`. The cause goes
  to hairpin's log, not to the model.

Some requests are refused without reaching Billet. Each refusal is a
`tool_result_response` with `is_error: true`, sent at once so the
harness does not wait out its per-call timeout:

| Request | Refusal |
|---|---|
| A tool the run did not declare, or one hairpin does not fulfil | `hairpin does not fulfil the tool "<name>"` — the same wording for both, so the answer reveals nothing about the run's configuration. |
| A memory tool while hairpin has no `-billet-addr` (only reachable when hairpin was restarted without it after the job was submitted) | `hairpin has no memory backend configured` |
| A `request_id` already accepted for a memory call on this run | `hairpin has already accepted a memory call with this request id` |
| More than 1000 memory calls on one run | `this job has exceeded hairpin's memory-call limit` |
| A fifth concurrent memory call on one run | `hairpin is already running the maximum number of concurrent memory calls for this job` |

A `request_id` longer than 128 bytes is not answered at all: it is
logged and dropped, and the harness times the call out. The counters
are per harness stream and are not persisted.

### Concurrency and timing

Each Billet call is bounded by a 10 second timeout and runs on its own
goroutine, so a slow Billet does not stall the event pump: the harness
keeps streaming text and heartbeats while a call is in flight, and the
answer is sent when it arrives. Up to four calls per run may be in
flight at once. Hairpin's shutdown waits for in-flight memory calls
after it has cancelled live runs, so a late answer is still recorded.

## Timeline events

Every request and its answer land on the job timeline, so the web UI
and `WatchJob` show memory traffic alongside the run:

| Event | Origin | Payload |
|---|---|---|
| `tool_result_request` | harness | The `HarnessEvent` as protobuf-JSON: `request_id`, `tool_use_id`, `tool_name`, `input`. `input` is a `bytes` field, so it appears base64-encoded; decoding it yields the model's JSON arguments. |
| `tool_result_response` | hairpin | The `ControlEvent` hairpin sent: `request_id`, `content`, `is_error`. |

The response is recorded whether or not delivery to the harness
succeeds. An answer that resolves after the harness has hung up is
exactly what an operator needs to see, and for `save_memory` it is the
only trace that a memory was written.

Recorded memory content therefore has a second home: `search_memory`
results and `save_memory` inputs sit in the job's Redis event stream,
with Redis's retention rather than Billet's, and are readable through
`WatchJob` and the web UI, including records other runs saved. Hairpin's
own log carries job ID, tool name, request ID, and withheld error
detail, never memory content.

## Trust posture

Memory is one namespace shared by every run hairpin proxies:

- `save_memory` content is model-authored and stored verbatim;
  `search_memory` returns it verbatim into another run's model context.
  A run can durably influence later runs, including runs on other
  profiles, and the recommended `search_memory` description asks the
  model to call it before starting work, so recalled content lands
  early and reads as trusted knowledge.
- Run one hairpin deployment per trust domain. Do not put a profile
  that processes third-party or untrusted text behind the same Billet
  as a profile whose runs carry credentials or egress-capable tooling.
- A profile can set `requiresApproval: true` on `save_memory` so that
  every write passes through the run's permission policy (a Cedar rule,
  or an `ask-upstream` `permission_request`). The reference profiles do
  not: memory writes dispatch like read-only built-ins.
- Reaching hairpin's `JobService` is equivalent to reaching Billet. A
  caller can submit a `run_config_json` declaring both tools and drive
  a run that reads and writes the namespace, so restrict hairpin's port
  to the audience that may use the memory. The API authenticates
  nobody; see [issue #5](https://github.com/rxbynerd/hairpin/issues/5).
- Billet's RPC endpoint authenticates nobody either. In the reference
  manifests a NetworkPolicy admits only hairpin's Pods, which is access
  control only on a CNI that enforces NetworkPolicy; treat that as a
  deployment prerequisite. Harness Jobs share Billet's namespace but
  carry the `stirrup` label, so the policy excludes them, and sandbox
  Pods sit in another namespace behind their own deny-all policy.
- Hairpin dials Billet over plaintext h2c, so memory content crosses
  the cluster network unencrypted, as does the control-plane stream it
  arrived on.

## Deployment

[`examples/k8s/billet.yaml`](../examples/k8s/billet.yaml) runs Billet
in the `hairpin` namespace with only its RPC listener enabled
(`--rpc --mcp=false --rpc-listen=:8141`), a `bolt` backend on a per-Pod
`emptyDir`, a NetworkPolicy admitting ingress from hairpin's Pods only,
and no API server token. Hairpin's Deployment adds
`--billet-addr=billet.hairpin.svc:8141`, and the reference `default`
profile declares the tools, so applying the reference manifests with
`kubectl apply -k` is enough for the default profile to use memory. For memory that
survives a Billet restart, replace the `emptyDir` with a
PersistentVolumeClaim.

### The development cluster

`just deploy` builds Billet when `${BILLET_DIR}/Containerfile` exists
(`BILLET_DIR` defaults to `../billet`, a sibling checkout), loads the
image into the kind node, and pins the Deployment to it. Without a
checkout it applies the manifest unchanged and the published image is
pulled.

The fake provider drives a run that searches memory, saves a memory
naming its own prompt, runs a sandbox command, and finishes by quoting
the search result back. `just memory-smoke-test` submits two such jobs
and asserts the second one's final text contains what the first saved,
then queries Billet directly through a port-forward to separate a
record Billet never received from one hairpin did not read back.
`deploy.sh` and both smoke tests run `kubectl` against a pinned
context, `HAIRPIN_KUBE_CONTEXT` (default `kind-<cluster>`), rather
than whatever the shell last selected.
