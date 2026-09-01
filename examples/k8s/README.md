# Kubernetes reference manifests

A minimal, applyable starting point for running hairpin on a cluster:
hairpin itself, the three identities involved in a run, a bare-bones
Redis, the profiles hairpin serves, and a placeholder Secret for
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
| `profiles.yaml` | ConfigMap | RunConfig profile templates, mounted at `--profiles`. |
| `hairpin.yaml` | Deployment + Service | hairpin itself. |
| `secret.yaml` | Secret | Placeholder provider API keys, exposed to harness Pods via `--harness-secrets`. Replace the value before applying, or generate the Secret out-of-band and remove it from `kustomization.yaml`. |
| `kustomization.yaml` | Kustomization | Applies all reference resources with namespaces ordered first. |

## What to edit before applying

- `hairpin.yaml`: `containers[0].image` — a built-and-pushed hairpin
  image. The stirrup harness and sandbox images already default to
  their published tags.
- `profiles.yaml`: the `default` profile's model and permission
  policy, or add profiles of your own.
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
