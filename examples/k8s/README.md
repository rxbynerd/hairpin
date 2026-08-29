# Kubernetes reference manifests

A minimal, applyable starting point for running hairpin on a cluster:
hairpin itself, the three identities involved in a run, a bare-bones
Redis, Billet for shared memory, steeplechase for the traces runs
emit, the profiles hairpin serves, a placeholder Secret for provider
API keys, and haybale, the git-credential proxy a sandbox reaches
through hairpin's sandbox-identity token issuer.

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
| `hairpin.yaml` | Deployment + Service | hairpin itself, including the `--sandbox-token-*` flags that make it a JWT issuer for haybale. |
| `secret.yaml` | Secret | Placeholder provider API keys, exposed to harness Pods via `--harness-secrets`. Replace the value before applying, or generate the Secret out-of-band and remove it from `kustomization.yaml`. |
| `sandbox-token.yaml` | Secret + ConfigMap | The signing keypair hairpin and haybale share: `key.pem` (hairpin, private) and `jwks.json` (haybale, public), generated together by `hairpin keygen`. Both are non-functional placeholders — see [Deploying haybale](#deploying-haybale). |
| `haybale.yaml` | ConfigMap ×2 + Secret + Deployment + Service | haybale, wired to a dev-only in-cluster gitea upstream until GitHub App credentials are configured — see [Deploying haybale](#deploying-haybale). |
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
  tools; a profile that omits them is opted out. The `git` profile needs
  a real `haybale`/upstream host wired up (or drop it if you have no use
  for proxied git access).
- `secret.yaml`: the placeholder `ANTHROPIC_API_KEY` value, or any
  other `secret://`-referenced keys your profiles need.
- `sandbox-token.yaml` and `haybale.yaml`'s `haybale-gitea-token`
  Secret: treat as **required**, not optional, if you apply
  `hairpin.yaml` and `haybale.yaml` as they ship — `key.pem` is not a
  real PEM, and hairpin's `--sandbox-token-key` is expected to reject an
  unparseable key at startup the same way haybale itself fails fast on a
  bad key or JWKS file. Generate a real pair with `hairpin keygen`, or
  drop both `sandbox-token.yaml` and `haybale.yaml` and remove the
  `--sandbox-token-*` flags from `hairpin.yaml` if you have no use for
  proxied git access.

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

## Deploying haybale

haybale (docs at [`~/Developer/haybale`](https://github.com/rxbynerd/haybale)
— a local, read-only checkout in this dev environment) is an
authenticating reverse proxy for Git smart HTTP: a sandbox Pod
authenticates to it with the short-lived JWT hairpin's
`--sandbox-token-*` issuer minted for that run, haybale checks a
default-deny policy, and swaps in an upstream credential the sandbox
never sees.

There is no published haybale image yet:

```sh
podman build -t localhost/haybale:dev <path-to-haybale-checkout>
```

`scripts/dev/haybale.sh` does this (and everything else below) for the
kind development loop; the rest of this section is the manual/production
path.

1. Generate the shared signing keypair and replace both halves of
   `sandbox-token.yaml`:

   ```sh
   ./hairpin keygen --out key.pem --jwks-out jwks.json
   kubectl -n hairpin create secret generic hairpin-sandbox-token-key \
     --from-file=key.pem=key.pem --dry-run=client -o yaml | kubectl apply -f -
   kubectl -n hairpin create configmap haybale-jwks \
     --from-file=jwks.json=jwks.json --dry-run=client -o yaml | kubectl apply -f -
   ```

   Rotating the key means repeating this and restarting **both**
   `hairpin` (to sign with the new key) and `haybale` (to trust it) —
   haybale reads `jwksFile` once at startup, with no hot reload.

2. Point `haybale.yaml`'s upstream at a real git host and real
   credentials. It ships wired to a dev-only in-cluster gitea using a
   static token (see `scripts/dev/gitea.yaml`) because GitHub App
   credentials are not available yet (blocked — see
   [`TODO.md`](../../TODO.md) Wave 6); the file has a commented
   `github-app` upstream block ready for when they are.

3. Narrow `haybale.yaml`'s `haybale-policy` ConfigMap from the shipped
   `hp-*` ceiling to the identities and repos your deployment actually
   needs, and set a real `HAYBALE_GITEA_TOKEN` (or equivalent) via
   `haybale.yaml`'s Secret.

4. Give a profile `executor.sandboxIdentity` and `executor.gitProxy` —
   see `profiles.yaml`'s `git` profile — so hairpin requests a token for
   the run and the harness rewrites git traffic through haybale.

### Network mode: a known gap, not a silent one

The `git` profile ships with `executor.network.mode: "none"`, which is
**not** the mode that is supposed to let a sandbox Pod reach haybale —
it is a documented fallback for a non-NetworkPolicy-enforcing cluster.
Reading stirrup's Kubernetes executor
(`harness/internal/executor/k8s_netpol.go` in a stirrup checkout) shows
why:

- `mode: "none"` installs a genuine deny-all `NetworkPolicy` selecting
  the sandbox Pod (`denyAllEgressPolicy`) — on a CNI that enforces
  NetworkPolicy (Cilium, Calico, ...) this blocks *everything*,
  including the in-cluster route to `haybale.hairpin.svc`, not just
  external egress.
- `mode: "allowlist"` is the mode actually designed for this, but it
  requires deploying stirrup's own `stirrup-egress-proxy` — a separate
  generic HTTP(S) CONNECT proxy, distinct from haybale — as its own
  Deployment in the *sandbox* namespace (`hairpin-sandboxes`), plus a
  namespace-scoped NetworkPolicy and an FQDN allowlist that would need
  to include haybale's own address (see
  `examples/k8s/egress-proxy/README.md` in the stirrup repo). That is a
  second proxy hop in front of a purpose-built, already-authenticated
  credential proxy — disproportionate for reaching one in-cluster
  service, and stirrup's own dev/kind loop
  (`stirrup/scripts/dev/`) does not exercise it either.

So the `git` profile only reaches haybale today on a CNI that does not
enforce NetworkPolicy — kind's default `kindnet` among them, which is
why the kind development loop (`scripts/dev/kind-up.sh`) works. A
NetworkPolicy-enforcing production cluster needs `allowlist` mode plus
stirrup's `examples/k8s/egress-proxy/` manifests deployed alongside the
sandbox namespace, with haybale's `host:port` in its allowlist —
tracked as a follow-up in `TODO.md` Wave 6 rather than built here.

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
