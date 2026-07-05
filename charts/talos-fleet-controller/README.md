# talos-fleet-controller Helm chart

Deploys the Talos Fleet Controller: a **Cluster API Runtime SDK extension
server** implementing the in-place update hooks (`CanUpdateMachine`,
`CanUpdateMachineSet`, `UpdateMachine`) that apply Talos machine-config
changes over the Talos API instead of reprovisioning machines.

```bash
helm install tfc charts/talos-fleet-controller \
  --set image.tag=main   # or an immutable sha-<commit> tag
```

> Only `main` and `sha-<commit>` image tags are published today (semver tags
> appear with the first tagged release). Always set `image.tag` — prefer an
> immutable `sha-*` tag in GitOps.

## What the binary is (and is not)

`cmd/main.go` starts a CAPI Runtime SDK webhook server (TLS, default port
9443). Its only flags are `--webhook-port`, `--webhook-cert-dir`,
`--profiler-address`, and standard logging flags. There is **no leader
election, no metrics endpoint, and no HTTP health endpoint** — probes use a
TCP socket check on the webhook port (the listener only comes up after the
serving certificate loads, so TCP connectivity implies a working TLS server).

The **FleetNodeSet CRD is installed but inert**: the current binary contains
no FleetNodeSet reconciler. CRs are declarative intent for the future
config-convergence feature (see `crds/fleet.talos.dev_fleetnodesets.yaml`).

## TLS serving certificate

The server does not self-generate certificates. It reads `tls.crt`/`tls.key`
from `webhook.certDir`, where the chart mounts the Secret named by
`webhook.certSecretName` (default `<fullname>-cert`). The pod cannot start
until that Secret exists.

### Option A — cert-manager (default, `certManager.enabled=true`)

The chart creates a self-signed `Issuer` and a `Certificate` whose Secret
includes `ca.crt`, which CAPI uses for `ExtensionConfig` caBundle injection.
Set `certManager.issuerRef` to use an existing issuer instead.

### Option B — manual Secret (`certManager.enabled=false`)

```bash
NS=tfc-system
SVC=<release-fullname>-webhook   # e.g. tfc-talos-fleet-controller-webhook
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
  -keyout tls.key -out tls.crt -days 3650 \
  -subj "/CN=${SVC}.${NS}.svc" \
  -addext "subjectAltName=DNS:${SVC}.${NS}.svc,DNS:${SVC}.${NS}.svc.cluster.local"
kubectl -n "$NS" create secret generic <release-fullname>-cert \
  --from-file=tls.crt --from-file=tls.key --from-file=ca.crt=tls.crt
```

Include `ca.crt` (for a self-signed cert, the cert itself) so CAPI can inject
the caBundle when you enable registration. Plan your own rotation.

## Registration with Cluster API (`extensionConfig.enabled`, default `false`)

The webhook Service (port 443 → 9443) is always created, but the
`ExtensionConfig` that registers the hooks with CAPI is gated behind
`extensionConfig.enabled` and **defaults to false**: it requires the CAPI
management cluster to run with the `RuntimeSDK` feature gate, and turning the
hooks on changes rollout behavior for every matching Cluster — a deliberate,
separate step. The chart annotates the `ExtensionConfig` with
`runtime.cluster.x-k8s.io/inject-ca-from-secret` pointing at the serving-cert
Secret.

Known limitation: the handlers persist an in-place update config cache in a
ConfigMap in the hardcoded namespace `talos-inplace-system`
(`internal/handlers/handlers.go`). Until that is derived from the pod
namespace, create that namespace before enabling registration.

## Values contract (0.2.0)

| Key | Default | Description |
| --- | --- | --- |
| `replicaCount` | `1` | Stateless server; >1 replica is fine behind the Service. |
| `image.repository` | `ghcr.io/sylphxai/talos-fleet-controller` | Image repo. |
| `image.tag` | `""` (appVersion) | Set explicitly; prefer `sha-<commit>`. |
| `namespace.create` / `namespace.name` | `true` / `tfc-system` | Deployment namespace. |
| `webhook.port` | `9443` | Extension server TLS port. |
| `webhook.certDir` | `/tmp/k8s-webhook-server/serving-certs` | Where `tls.crt`/`tls.key` are read. |
| `webhook.certSecretName` | `""` (`<fullname>-cert`) | TLS Secret mounted at `certDir`. |
| `certManager.enabled` | `true` | Issue serving cert via cert-manager. |
| `certManager.issuerRef` | `{}` | Existing issuer instead of chart self-signed Issuer. |
| `service.port` | `443` | Webhook Service port (targets `webhook.port`). |
| `extensionConfig.enabled` | `false` | Create the CAPI `ExtensionConfig` (requires `RuntimeSDK=true`). |
| `extensionConfig.name` | `""` (`<fullname>`) | ExtensionConfig name. |
| `extensionConfig.namespaceSelector` | `{}` | Limit Clusters that may use the extension. |
| `extensionConfig.settings` | `{}` | Settings passed to handlers. |
| `controller.profilerAddress` | `""` | pprof bind address (empty = disabled). |
| `controller.extraArgs` / `controller.extraEnv` | `[]` | Extra args / env. |
| `talosServiceAccount.*` | create `tfc-talos-api`, `os:admin` | Talos API access via `talos.dev` ServiceAccount; the generated Secret is mounted and exported as `TALOSCONFIG`. |
| `rbac.create` | `true` | Minimal ClusterRole: `machines` get, `nodes` get, `configmaps` get/create/update. |
| `livenessProbe` / `readinessProbe` | TCP on `webhook` | No HTTP healthz exists in the binary. |

Removed in 0.2.0 (fed flags the binary no longer has —
`--leader-elect`, `--health-probe-bind-address`, `--metrics-bind-address`,
`--metrics-secure`): `controller.leaderElect`,
`controller.healthProbeBindAddress`, `controller.metricsBindAddress`,
`controller.metricsSecure`.

## Prerequisites

- Talos nodes with `machine.features.kubernetesTalosAPIAccess` enabled, roles
  `[os:admin]`, allowed namespace = `namespace.name` (the Talos CRD controller
  then fulfills the `tfc-talos-api` Secret).
- cert-manager (default cert flow) or a manually created TLS Secret.
- For registration only: CAPI with the `RuntimeSDK` feature gate enabled.
