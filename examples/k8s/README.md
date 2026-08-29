# Kubernetes reference manifests

A minimal, applyable starting point for running hairpin on a cluster:
hairpin itself, the RBAC it needs to launch harness Jobs
(`internal/launcher/k8s.go`), a bare-bones Redis, and a placeholder
Secret for provider API keys.

## Files

| File | Kind | Purpose |
|---|---|---|
| `namespace.yaml` | Namespace | The `hairpin` namespace everything below lives in. |
| `rbac.yaml` | ServiceAccount + Role + RoleBinding | The `hairpin` identity and the batch/v1 Jobs verbs the launcher needs: create, get, list, watch, delete. |
| `redis.yaml` | Deployment + Service | Single-replica, unpersisted Redis for `internal/store/redisstore`. Fine for a kind cluster; swap for a managed instance otherwise. |
| `hairpin.yaml` | Deployment + Service | hairpin itself, flags wired to the k8s launcher and the Redis above. |
| `secret.yaml` | Secret | Placeholder provider API keys, exposed to harness Pods via `--k8s-env-from-secrets`. Replace the value before applying, or generate the Secret out-of-band and drop this file. |

## What to edit before applying

- `hairpin.yaml`: `containers[0].image` (a built-and-pushed hairpin
  image) and `--k8s-image` (the stirrup image the launcher runs as
  harness Jobs).
- `secret.yaml`: the placeholder `ANTHROPIC_API_KEY` value, or any
  other `secret://`-referenced keys your RunConfig profiles need.

## Apply order

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

## Submitting a job

Once the `hairpin` Service is up, port-forward or exec in from
another Pod in the cluster and call `JobService/SubmitJob` (connect-go
serves JSON and gRPC on the same h2c port):

```sh
kubectl -n hairpin port-forward svc/hairpin 8130:8130

curl -s http://localhost:8130/hairpin.v1.JobService/SubmitJob \
  -H 'Content-Type: application/json' \
  -d '{"prompt": "summarize the open issues", "profile": "default"}'
```

The response carries the job ID; poll `GetJob` or open the web UI at
`http://localhost:8130/` to watch it run.

## Trust posture

As noted in [`docs/design.md`](../../docs/design.md), hairpin and the
harnesses it launches communicate over plaintext, unauthenticated
gRPC, and the JobService API and web UI carry no authentication of
their own. Keep the `hairpin` Service cluster-internal (no Ingress in
this example) and front it with your own authenticating proxy if it
needs to be reachable from outside the cluster.
