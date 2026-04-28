# dicode Helm chart

[dicode](https://github.com/dicode-ayo/dicode-core) is a single-binary
GitOps task orchestrator that watches a git repo of JS/TS/Python/Docker
task scripts and runs them on cron schedules, webhooks, manual
triggers, and chained triggers.

This chart deploys the [`dicode-core`](https://github.com/dicode-ayo/dicode-core)
daemon to a Kubernetes cluster.

## TL;DR

```bash
git clone https://github.com/dicode-ayo/dicode-core.git
helm install dicode ./dicode-core/deploy/helm/dicode \
  --create-namespace --namespace dicode \
  --set secret.create=true \
  --set secret.values.ANTHROPIC_API_KEY=$ANTHROPIC_API_KEY
```

Then:

```bash
kubectl -n dicode port-forward svc/dicode 8080:8080
open http://localhost:8080
```

## Prerequisites

- Kubernetes **1.27** or later
- Helm **3.8** or later
- A default `StorageClass` (or set `persistence.enabled=false` for
  ephemeral demos)
- The published Docker image (`ghcr.io/dicode-ayo/dicode-core` or
  `dicodeayo/dicode-core`) — first published on the next
  release-please tag of dicode-core

## Install

From the cloned repo:

```bash
helm install dicode ./deploy/helm/dicode \
  --create-namespace --namespace dicode
```

A future follow-up will publish the chart to a Helm repository so you
can `helm repo add` instead.

## Upgrade

```bash
helm upgrade dicode ./deploy/helm/dicode --namespace dicode
```

The deployment uses the `Recreate` strategy because the data PVC is
RWO (a rolling upgrade can't attach the volume to two pods
simultaneously). Expect a brief gap in availability.

## Uninstall

```bash
helm uninstall dicode --namespace dicode
```

> **The PVC is kept on uninstall.** It carries the
> `helm.sh/resource-policy: keep` annotation so your SQLite database,
> master key, and `dicode.yaml` survive a `helm uninstall`. Remove it
> manually if you want a clean wipe:
>
> ```bash
> kubectl --namespace dicode delete pvc dicode-data
> ```

## Configuration

| Key | Default | Description |
|---|---|---|
| `image.repository` | `ghcr.io/dicode-ayo/dicode-core` | Image repository. Docker Hub mirror: `dicodeayo/dicode-core`. |
| `image.tag` | `""` | Image tag. Empty = use `Chart.appVersion`. |
| `image.pullPolicy` | `IfNotPresent` | Pull policy. |
| `imagePullSecrets` | `[]` | Pull secrets for private registries. |
| `replicaCount` | `1` | **Must be 1** until leader election lands; `> 1` may run cron triggers more than once. |
| `nameOverride` | `""` | Override chart name in resource names. |
| `fullnameOverride` | `""` | Override fully-qualified name. |
| `serviceAccount.create` | `true` | Create a dedicated ServiceAccount. |
| `serviceAccount.annotations` | `{}` | ServiceAccount annotations (IRSA / Workload Identity). |
| `serviceAccount.name` | `""` | ServiceAccount name. Empty = `<release>-dicode`. |
| `podSecurityContext` | `runAsNonRoot+UID 65532+seccomp` | Pod-level security context. |
| `containerSecurityContext` | `readOnlyRootFilesystem+drop ALL caps` | Container-level security context. |
| `service.type` | `ClusterIP` | Service type. |
| `service.port` | `8080` | Service port. |
| `service.annotations` | `{}` | Service annotations (cloud LB). |
| `ingress.enabled` | `false` | Render an Ingress. |
| `ingress.className` | `""` | IngressClass name. |
| `ingress.annotations` | `{}` | Ingress annotations. |
| `ingress.hosts` | `[{ host: dicode.local, paths: [{ path: /, pathType: Prefix }] }]` | Ingress hosts. |
| `ingress.tls` | `[]` | Ingress TLS. |
| `persistence.enabled` | `true` | Provision a PVC at `/data`. |
| `persistence.storageClass` | `""` | StorageClass. `""` = default; `-` = none (pre-bound PV). |
| `persistence.accessModes` | `[ReadWriteOnce]` | PVC access modes. |
| `persistence.size` | `5Gi` | PVC size. |
| `persistence.annotations` | `{}` | PVC annotations (chart always adds `helm.sh/resource-policy: keep`). |
| `persistence.existingClaim` | `""` | Use an existing PVC instead of creating one. |
| `resources.limits.cpu` | `500m` | CPU limit. |
| `resources.limits.memory` | `512Mi` | Memory limit. |
| `resources.requests.cpu` | `100m` | CPU request. |
| `resources.requests.memory` | `128Mi` | Memory request. |
| `livenessProbe` | `httpGet /healthz @ http` | Liveness probe (probes the unauth `/healthz` route). |
| `readinessProbe` | `httpGet /healthz @ http` | Readiness probe. |
| `config` | `""` | Optional `dicode.yaml` body. When set, mounted via ConfigMap at `/data/dicode.yaml`. |
| `secret.create` | `false` | Render a Secret from `secret.values`. |
| `secret.values` | `{}` | Map of env-var → value (passphrase, AI keys, OAuth creds). |
| `secret.existingSecret` | `""` | Use an existing Secret (skips chart-managed Secret). |
| `env` | `[]` | Extra literal env vars. |
| `envFrom` | `[]` | Extra envFrom sources. |
| `nodeSelector` | `{}` | Pod node selector. |
| `tolerations` | `[]` | Pod tolerations. |
| `affinity` | `{}` | Pod affinity. |
| `podAnnotations` | `{}` | Annotations on every pod. |
| `podLabels` | `{}` | Labels on every pod. |
| `terminationGracePeriodSeconds` | `60` | SIGTERM-to-SIGKILL grace period. The daemon flushes SQLite on SIGTERM. |

See [`values.yaml`](./values.yaml) for the canonical, commented version.

### Custom `dicode.yaml`

Two ways:

**1. Set `config` in your values file** — the chart writes a ConfigMap and mounts it at `/data/dicode.yaml`:

```yaml
# values-prod.yaml
config: |
  data_dir: /data
  database:
    type: sqlite
    path: /data/data.db
  server:
    port: 8080
    mcp: true
  sources:
    - type: git
      id: tasks
      url: https://github.com/me/dicode-tasks
      branch: main
      poll_interval: 60s
  log_level: info
```

```bash
helm install dicode ./deploy/helm/dicode -f values-prod.yaml
```

> **Heads up.** When you set `config`, **don't also leave a stale
> `dicode.yaml` in the PVC**. The mount via subPath shadows whatever
> the daemon's first-run onboarding wrote previously, but
> onboarding-mode hashes/passphrases stick around. Either let
> onboarding own the file (leave `config: ""`) or fully manage it via
> the chart (set `config`).

**2. Let onboarding write its own** — leave `config: ""` (the
default). On first start, the daemon creates `/data/dicode.yaml` plus
`master.key` and `data.db` next to it. They survive pod restarts via
the PVC.

### Secrets

The daemon reads its passphrase, AI keys, and OAuth creds from
environment variables. Two ways to wire them up:

**Chart-managed (good for dev/test):**

```yaml
secret:
  create: true
  values:
    DICODE_PASSPHRASE: "change-me-in-production"
    ANTHROPIC_API_KEY: "sk-ant-..."
    OPENAI_API_KEY: "sk-..."
    GITHUB_TOKEN: "ghp_..."
```

The values land in your `helm install --set ...` shell history if you
pass them on the command line — prefer a values file mode 0600 or an
external secret store for anything beyond local dev.

**External (recommended for production):**

```yaml
secret:
  existingSecret: my-prod-dicode-secret
```

Manage that Secret with whatever tooling you prefer
(SealedSecrets, External Secrets Operator, Vault Agent, etc.). Every
key in the Secret is exposed as an env var on the daemon container.

### Ingress

```yaml
ingress:
  enabled: true
  className: nginx
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
    nginx.ingress.kubernetes.io/proxy-body-size: 10m
  hosts:
    - host: dicode.example.com
      paths:
        - path: /
          pathType: Prefix
  tls:
    - secretName: dicode-tls
      hosts:
        - dicode.example.com
```

### Resource sizing

The defaults (100m / 128Mi requests, 500m / 512Mi limits) suit a small
fleet of cron tasks. For Docker-runtime tasks running large images,
bump CPU and memory generously — the daemon spawns the container
inline.

## Health probes

The chart points liveness and readiness at `GET /healthz`, which
returns `{"status":"ok","version":"<x>"}` and bypasses auth. See
[`pkg/webui/server.go`](https://github.com/dicode-ayo/dicode-core/blob/main/pkg/webui/server.go)
for the route definition and `pkg/webui/healthz_test.go` for the
auth-bypass guarantee.

## Testing

```bash
helm test dicode --namespace dicode
```

This runs a one-shot busybox pod that wgets `/healthz` against the
service and asserts the response contains `"status":"ok"`. The pod
inherits the same nonroot + read-only-root + drop-ALL-caps stance as
the daemon.

## Limitations

- **Single replica only.** The daemon does not yet have leader
  election (issue [#215](https://github.com/dicode-ayo/dicode-core/issues/215)
  follow-up). Setting `replicaCount > 1` will run cron triggers more
  than once and is **not** supported. If you need horizontal scaling
  before that lands, deploy multiple Helm releases each with a
  different source.
- **No Postgres support yet.** SQLite + RWO PVC is the only persistence
  story. Postgres tracked in
  [#216](https://github.com/dicode-ayo/dicode-core/issues/216).
- **No NetworkPolicy.** If you have a cluster-wide default-deny
  policy, vendor in your own — a follow-up issue will add an opt-in
  NetworkPolicy template.
- **Chart not yet published to a Helm repo.** Install from the repo
  source. A `chart-releaser-action` follow-up will publish to
  `dicode-ayo.github.io/charts`.
- **Chart not signed.** Cosign signing follows the publishing step.

## Contributing

The chart lives at `deploy/helm/dicode/` in
[`dicode-ayo/dicode-core`](https://github.com/dicode-ayo/dicode-core).
Every PR touching `deploy/helm/**` runs `helm lint --strict` and
`kubeconform` against the rendered manifests in CI
(`.github/workflows/helm-lint.yml`). Please run the same locally
before opening a PR:

```bash
helm lint --strict deploy/helm/dicode
helm template smoke deploy/helm/dicode | kubeconform -strict -summary -kubernetes-version 1.31.0 -schema-location default
```
