# Kubernetes reference manifests

A minimal, applyable starting point for running hairpin on a cluster:
hairpin itself, the three identities involved in a run, a bare-bones
Redis, Billet for shared memory, steeplechase for the traces runs
emit, the profiles hairpin serves, and a placeholder Secret for
provider API keys.

Full narrative, including the trust boundary the two namespaces draw
and what the harness's RBAC is for, is in
[`docs/deployment.md`](../../docs/deployment.md).

## Files

| File | Kind | Purpose |
|---|---|---|
| `namespace.yaml` | Namespace ×2 | `hairpin` (server, Redis, harness Jobs) and `hairpin-sandboxes` (the Pods agent commands run in). |
| `rbac.yaml` | ServiceAccount + Role + RoleBinding | The `hairpin` identity and the `create` verb its Job launcher uses. |
| `rbac-sandbox.yaml` | ServiceAccount ×2 + Role + RoleBinding | The `stirrup-harness` identity that creates and execs into sandbox Pods, and the token-less `stirrup-sandbox` identity those Pods run as. |
| `redis.yaml` | Deployment + Service | Single-replica, unpersisted Redis for `internal/store/redisstore`. Fine for a kind cluster; swap for a managed instance otherwise. |
| `billet.yaml` | Deployment + Service + NetworkPolicy | Billet, the store behind the `search_memory` and `save_memory` tools hairpin fulfils. Its RPC endpoint authenticates nobody, so the NetworkPolicy admits hairpin's Pods only. |
| `steeplechase.yaml` | Deployment + Service | The OTLP collector each run's `trace_emitter` points at, via hairpin's `--harness-telemetry-endpoint`. Ships with no `--sink`, so traces land on stdout — `kubectl -n hairpin logs deploy/steeplechase`. |
| `profiles.yaml` | ConfigMap | RunConfig profile templates, mounted at `--profiles`. |
| `hairpin.yaml` | Deployment + Service | hairpin itself. |
| `secret.yaml` | Secret | Placeholder provider API keys, exposed to harness Pods via `--harness-secrets`. Replace the value before applying, or generate the Secret out-of-band and remove it from `kustomization.yaml`. |
| `kustomization.yaml` | Kustomization | Applies all reference resources with namespaces ordered first. |

## What to edit before applying

- `hairpin.yaml`: `containers[0].image` — a built-and-pushed hairpin
  image. The stirrup harness and sandbox images already default to
  their published tags; the memory tools need a harness built from
  [stirrup PR #586](https://github.com/rxbynerd/stirrup/pull/586)
  until it merges.
- `billet.yaml`: `containers[0].image` — `ghcr.io/rxbynerd/billet:latest`
  is not published until
  [billet PR #1](https://github.com/rxbynerd/billet/pull/1) merges, so
  point this at a Billet image built from that branch.
- `steeplechase.yaml`: `containers[0].image` —
  `ghcr.io/rxbynerd/steeplechase:latest` is not published yet (a pull
  returns 403), so point this at an image you have built from
  [steeplechase](https://github.com/rxbynerd/steeplechase). Drop the
  file and `--harness-telemetry-endpoint` to run without run traces.
- `profiles.yaml`: the `default` profile's model and permission
  policy, or add profiles of your own. The profile declares the memory
  tools; a profile that omits them is opted out.
- `secret.yaml`: the placeholder `ANTHROPIC_API_KEY` value, or any
  other `secret://`-referenced keys your profiles need.

The namespace and advertise address need no editing: hairpin reads its
namespace from its projected ServiceAccount and advertises
`hairpin.<namespace>.svc` on its listen port.

## Apply

```sh
kubectl apply -k examples/k8s/
```

Use the Kustomization on a new cluster so namespaces are created before
the resources inside them. Applying the directory with `-f` processes
files lexically and can reach namespaced resources first.

## Telemetry

`hairpin.yaml` passes `--harness-telemetry-endpoint=steeplechase.hairpin.svc:4317`,
so a submitted RunConfig that names no `trace_emitter` of its own gets
one pointed at steeplechase, and the harness exports the run's trace
there. A profile that sets its own `trace_emitter` keeps it.

Hairpin's own traces and metrics are a separate switch — `--telemetry`,
off by default — and can be pointed at the same steeplechase or
somewhere else entirely. See
[`docs/observability.md`](../../docs/observability.md).

To forward out of the cluster as well, add `--sink` DSNs to
`steeplechase.yaml`'s `args` (repeatable; every sink receives every
payload):

```yaml
args:
  - --stdout-format=grouped
  - --sink=otlp+grpc://collector.example.com:4317
  - --sink=mqtt://$(MQTT_USER):$(MQTT_PASSWORD)@mqtt.example.com:1883/hairpin
```

steeplechase reads no environment variables of its own, but Kubernetes
expands `$(VAR)` in `args` from the container's `env`, so sink
credentials can live in a Secret instead of the DSN literal:

```yaml
env:
  - name: MQTT_USER
    valueFrom: { secretKeyRef: { name: steeplechase-sink, key: mqtt-user } }
  - name: MQTT_PASSWORD
    valueFrom: { secretKeyRef: { name: steeplechase-sink, key: mqtt-password } }
```

## Submitting a job

Once the `hairpin` Service is up, port-forward or exec in from another
Pod in the cluster and call `JobService/SubmitJob` (connect-go serves
JSON and gRPC on the same h2c port):

```sh
kubectl -n hairpin port-forward svc/hairpin 8130:8130

curl -s http://localhost:8130/hairpin.v1.JobService/SubmitJob \
  -H 'Content-Type: application/json' \
  -d '{"prompt": "summarize the open issues", "profile": "default"}'
```

The response carries the job ID; poll `GetJob` or open the web UI at
`http://localhost:8130/` to watch it run.

For a throwaway cluster that needs no API key at all, see
[`scripts/dev/`](../../scripts/dev/) — `just kind-up && just deploy &&
just smoke-test`.

## Trust posture

As noted in [`docs/design.md`](../../docs/design.md), hairpin uses a
plaintext h2c listener. Per-job bearer tokens authenticate harness job
claims, but the JobService API and web UI do not authenticate callers.
Keep the `hairpin` Service cluster-internal (no Ingress in this example)
and add authenticated TLS at an ingress, or mesh mTLS, before making it
reachable outside the cluster.

steeplechase's ingest ports are the same: they accept OTLP from anything
that can reach them, with no authentication or TLS, so its Service stays
`ClusterIP`. Authentication on the way *out* belongs in a `--sink` DSN,
not on the ingest side.
