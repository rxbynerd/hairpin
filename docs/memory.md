# Shared memory with Billet

Hairpin can give every run it launches two extra tools, `search_memory`
and `save_memory`, backed by [Billet](https://github.com/rxbynerd/billet),
the Equestrianism suite's memory sidecar. Knowledge one session saves is
searchable by the next, so runs launched by the same hairpin build on
one another's work.

The tools are **control-plane-fulfilled**: the harness never talks to
Billet. A call travels up the `RunTask` stream the harness already
holds, hairpin proxies it to Billet's `billet.v1.MemoryService` over
connect-go, and the result travels back down the same stream. The agent
environment gains no network path to the knowledge system, which keeps
stirrup's no-direct-egress posture intact and matches Billet's "proxied"
deployment model (`billet serve --rpc --mcp=false`).

```
harness ──tool_result_request{tool_name:"search_memory", input}──▶ hairpin
   ▲                                                                 │
   │                                              SearchMemory (gRPC/h2c)
   │                                                                 ▼
   └──tool_result_response{content, is_error}◀──────────────────── billet
```

## Configuration

| Flag | Default | Meaning |
|---|---|---|
| `-billet-addr` | *(empty)* | `host:port` of Billet's RPC listener, dialled with plaintext gRPC (HTTP/2 prior knowledge). Empty disables memory: submits that declare the memory tools are rejected. |

Billet binds its memory namespace once at startup; every job hairpin
proxies shares that namespace. That is the point — sessions share
knowledge — but it also means a hairpin deployment is one memory scope.
Run separate Billet instances behind separate hairpins to partition.

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

Hairpin matches on the tool **name**: `search_memory` and `save_memory`
are the only control-plane tools it fulfils. `SubmitJob` rejects a
RunConfig that declares any other control-plane tool name (hairpin could
never answer it, and the harness would block for the per-call timeout on
every use), and rejects the memory tools when `-billet-addr` is unset.
Both checks happen before a Job is created, in the same preflight that
validates executor coordinates.

The `description` text is what steers the model; a profile that wants
sessions to share knowledge should say so there, or in
`systemPromptOverride`.

## Tool contracts

Inputs and outputs mirror Billet's own MCP tool surface, so a model sees
the same contract whether it reaches Billet directly or through hairpin.

| Tool | Input | Output (`content`) |
|---|---|---|
| `search_memory` | `{"query": string, "limit"?: int}` | `{"records": [{"memory_id", "content", "score", "created_at"}]}` |
| `save_memory` | `{"content": string, "kind"?: "event" \| "fact"}` | `{"memory_id": string, "accepted": bool}` |

Errors reach the model as a tool failure (`is_error: true`):

- Malformed input (not a JSON object, missing `query`/`content`, an
  unknown `kind`): the message names the field.
- Billet rejected the call (`INVALID_ARGUMENT`, `RESOURCE_EXHAUSTED`
  when the budget is spent): Billet's message, verbatim.
- Billet unreachable or any other failure: a fixed generic message.
  Detail goes to hairpin's log, not to the model.
- A `tool_result_request` for a tool hairpin does not fulfil, or a
  memory tool when memory is disabled: an explicit refusal, sent
  immediately so the harness does not wait out its timeout.

Hairpin bounds each Billet call with a 10 second timeout, well inside
stirrup's default per-call async timeout.

## Timeline events

Every request and its answer land on the job timeline, so the web UI and
`WatchJob` show memory traffic alongside the run:

| Event | Origin | Payload |
|---|---|---|
| `tool_result_request` | harness | The `HarnessEvent` verbatim: `request_id`, `tool_use_id`, `tool_name`, `input`. |
| `tool_result_response` | hairpin | The `ControlEvent` hairpin sent: `request_id`, `content`, `is_error`, `reason`. |

## Deployment

[`examples/k8s/billet.yaml`](../examples/k8s/billet.yaml) runs Billet
in the `hairpin` namespace with only its RPC listener enabled, a `bolt`
backend on a per-Pod volume, and a NetworkPolicy admitting ingress from
hairpin's Pods only. Harness Jobs run in the same namespace but cannot
reach Billet, and sandbox Pods are in another namespace behind their own
deny-all policy. hairpin's Deployment adds
`--billet-addr=billet.hairpin.svc:8141`.

Billet's RPC endpoint authenticates nobody, which is why the
NetworkPolicy matters: anything that can reach the port can read and
write the shared memory. For a persistent memory across Billet restarts,
replace the `emptyDir` with a PersistentVolumeClaim.

On the development cluster, `just deploy` builds Billet from a sibling
checkout (`BILLET_DIR`, default `../billet`) when one exists and
otherwise pulls the published image. The fake provider drives a run that
searches memory, saves a memory, runs a sandbox command, and finishes;
`just memory-smoke-test` submits two such jobs and asserts the second
one's `search_memory` result contains what the first one saved.
