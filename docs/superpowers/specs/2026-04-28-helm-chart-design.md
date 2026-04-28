# Helm chart for dicode-core (issue #215)

## Goal
Ship a production-ready Helm chart at `deploy/helm/dicode/` consuming the
Docker image from PR #227 (`dicodeayo/dicode-core` / `ghcr.io/dicode-ayo/dicode-core`).
Closes #215.

## Scope cuts (vs. the issue body)

| Issue ask | Disposition | Reason |
|---|---|---|
| Chart layout, values, templates, schema | **Ship** | Core MVP |
| `replicaCount: 1` default | **Ship** with note that `> 1` may duplicate cron runs | Leader election unimplemented |
| PVC at `/data`, 5 Gi default, configurable storageClass | **Ship** | Core persistence story |
| Hardened pod / container security context | **Ship verbatim** | Quality bar |
| `helm lint` in CI | **Ship** | Workflow `.github/workflows/helm-lint.yml` |
| `helm test` connection probe | **Ship** | Single template `tests/test-connection.yaml` using busybox `wget` against `/healthz` |
| README in chart dir | **Ship** (hand-written, no helm-docs dep) | Tighter |
| Docs page | **Ship** as a Kubernetes section appended to `docs/concepts/deployment.md` | Repo has `docs/`, not `docs-src/` |
| Leader election (`pkg/cluster/lease.go`) | **Defer** to follow-up issue | Requires Go code |
| `kind` / `k3d` smoke test in CI | **Defer** to follow-up issue | Risky to ship blind without iteration; helm-lint + template render is the floor |
| `chart-releaser-action` to GH Pages | **Defer** to follow-up issue | Needs new branch + workflow + repo settings |
| Cosign signing | **Defer** to follow-up issue | Bundles with chart-releaser story |
| artifacthub.io listing | **Defer** to follow-up issue | Depends on chart-releaser hosting |
| NetworkPolicy template | **Defer** to follow-up issue | Pre-MVP; users with strict policies tend to vendor in their own |
| Postgres support | **Out of scope** (#216) | Separate epic |

## Image source of truth
- Docker Hub: `dicodeayo/dicode-core`
- GHCR: `ghcr.io/dicode-ayo/dicode-core`
- Default `image.repository: ghcr.io/dicode-ayo/dicode-core` (no auth required for public pulls)
- Default `image.tag: ""` so `Chart.appVersion` drives the tag — chart and image stay in lockstep on every release-please cut
- `image.pullPolicy: IfNotPresent`
- Image runs as UID/GID 65532 (distroless `nonroot`), exposes 8080, sets `DICODE_DATA_DIR=/data`, `CMD ["daemon", "--config", "/data/dicode.yaml"]`

## Health check contract
- `GET /healthz` returns 200 + JSON `{"status":"ok","version":"<x>"}`
- Bypasses auth (verified by `pkg/webui/healthz_test.go` `TestHealthzBypassesAuth`)
- Used by both `livenessProbe` and `readinessProbe`

## Chart structure

```
deploy/helm/dicode/
├── .helmignore
├── Chart.yaml
├── values.yaml
├── values.schema.json
├── README.md
└── templates/
    ├── _helpers.tpl
    ├── NOTES.txt
    ├── deployment.yaml
    ├── service.yaml
    ├── configmap.yaml         # optional, gated on .Values.config != ""
    ├── secret.yaml            # optional, gated on .Values.secret.create
    ├── pvc.yaml               # optional, gated on .Values.persistence.enabled
    ├── serviceaccount.yaml    # optional, gated on .Values.serviceAccount.create
    ├── ingress.yaml           # optional, gated on .Values.ingress.enabled
    └── tests/
        └── test-connection.yaml
```

## values.yaml schema (canonical)

```yaml
# -- dicode-core image
image:
  repository: ghcr.io/dicode-ayo/dicode-core
  tag: ""                 # falls back to .Chart.AppVersion
  pullPolicy: IfNotPresent
imagePullSecrets: []

# -- Replica count. Multi-replica is not yet supported because the
# daemon has no leader election; running >1 may duplicate cron runs.
replicaCount: 1

# -- Optional. Override the chart name in resource names.
nameOverride: ""
fullnameOverride: ""

# -- ServiceAccount.
serviceAccount:
  create: true
  annotations: {}
  name: ""

# -- Pod-level security context (matches distroless nonroot UID 65532).
podSecurityContext:
  runAsNonRoot: true
  runAsUser: 65532
  runAsGroup: 65532
  fsGroup: 65532
  seccompProfile:
    type: RuntimeDefault

# -- Container-level security context.
containerSecurityContext:
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  runAsNonRoot: true
  runAsUser: 65532
  runAsGroup: 65532
  capabilities:
    drop: ["ALL"]

# -- Service.
service:
  type: ClusterIP
  port: 8080
  annotations: {}

# -- Ingress.
ingress:
  enabled: false
  className: ""
  annotations: {}
  hosts:
    - host: dicode.local
      paths:
        - path: /
          pathType: Prefix
  tls: []

# -- Persistence (PVC backing /data: SQLite, master.key, dicode.yaml).
persistence:
  enabled: true
  storageClass: ""
  accessModes: ["ReadWriteOnce"]
  size: 5Gi
  annotations: {}
  # If set, mounts an existing PVC instead of creating one.
  existingClaim: ""

# -- Resource requests/limits.
resources:
  limits:
    cpu: 500m
    memory: 512Mi
  requests:
    cpu: 100m
    memory: 128Mi

# -- Probes (point at /healthz).
livenessProbe:
  httpGet:
    path: /healthz
    port: http
  initialDelaySeconds: 15
  periodSeconds: 20
  timeoutSeconds: 3
  failureThreshold: 3
readinessProbe:
  httpGet:
    path: /healthz
    port: http
  initialDelaySeconds: 5
  periodSeconds: 10
  timeoutSeconds: 3
  failureThreshold: 3

# -- Optional dicode.yaml. When non-empty, rendered into a ConfigMap
# and mounted at /data/dicode.yaml. When empty, the daemon's first-run
# onboarding writes its own. Note: do NOT also keep an existing
# dicode.yaml inside the PVC — onboarding will skip and the mounted
# ConfigMap copy will shadow it on next startup, which is confusing.
config: ""

# -- Secret (passphrase, AI keys, OAuth creds, etc.) — exported as env
# vars on the daemon container. Populate either via secrets.values or
# point at a pre-existing secret.
secret:
  create: false
  # Map of envVarName -> value. Rendered into a Secret with these keys.
  values: {}
  # When non-empty, the chart skips creating a Secret and reads env
  # vars from this existing secret.
  existingSecret: ""

# -- Extra environment variables (literal name/value pairs). Useful for
# DICODE_HEADLESS or any non-secret toggle.
env: []
#  - name: DICODE_HEADLESS
#    value: "true"

# -- Extra envFrom sources (e.g. for additional ConfigMaps/Secrets).
envFrom: []

# -- Pod scheduling.
nodeSelector: {}
tolerations: []
affinity: {}
podAnnotations: {}
podLabels: {}

# -- Termination grace period (daemon flushes SQLite on SIGTERM).
terminationGracePeriodSeconds: 60
```

## values.schema.json
- `$schema: http://json-schema.org/draft-07/schema#` (Helm's supported draft)
- Validates types (e.g., `replicaCount: integer >= 0`, `service.port: integer 1..65535`, `persistence.size: string` matching a quantity regex, `secret.create: boolean`)
- Rejects misspellings (`additionalProperties: false` at the top level), but allows pass-through under loose subobjects (`resources`, `nodeSelector`, `affinity`, `podAnnotations`, `tolerations`, `imagePullSecrets`, `service.annotations`, `ingress.annotations`, `persistence.annotations`, `secret.values`, `envFrom`)

## Templates — key behaviors

### `_helpers.tpl`
- `dicode.name` — chart name (or `nameOverride`)
- `dicode.fullname` — release-name + chart-name (or `fullnameOverride`)
- `dicode.chart` — `name-version`
- `dicode.labels` — standard set: `helm.sh/chart`, `app.kubernetes.io/{name,instance,version,managed-by}`
- `dicode.selectorLabels` — `app.kubernetes.io/{name,instance}` only
- `dicode.serviceAccountName` — overrides + create logic
- `dicode.image` — `{repository}:{tag | default appVersion}`

### `deployment.yaml`
- StatefulSet vs Deployment? Stays a Deployment. SQLite + PVC is fine on a Deployment with `RWO` PVC and `replicas: 1`. StatefulSet adds VolumeClaimTemplates per replica which is the wrong model for a singleton with shared state.
- `strategy: Recreate` because RWO PVCs cannot be attached to two pods during a rolling update.
- Mounts:
  - PVC (or emptyDir if `persistence.enabled: false`) at `/data`
  - emptyDir at `/tmp` (required because root FS is read-only)
  - ConfigMap subPath-mounted at `/data/dicode.yaml` if `.Values.config != ""`
- Probes use the named port `http` to keep service+container in sync.
- `envFrom` includes the chart-managed Secret (if `secret.create` or `existingSecret` set).

### `service.yaml`
- Always rendered. Type/port/annotations from values.

### `pvc.yaml`
- Rendered when `persistence.enabled && existingClaim == ""`.
- `helm.sh/resource-policy: keep` annotation so `helm uninstall` doesn't nuke task data; documented in the README.

### `configmap.yaml`
- Rendered when `.Values.config != ""`.
- Single key `dicode.yaml`.

### `secret.yaml`
- Rendered when `secret.create == true`.
- `data:` map with base64-encoded values.

### `serviceaccount.yaml`
- Rendered when `serviceAccount.create == true`.

### `ingress.yaml`
- Rendered when `ingress.enabled == true`.
- Uses `networking.k8s.io/v1` (Helm chart `kubeVersion >= 1.27` per Chart.yaml).

### `tests/test-connection.yaml`
- `helm.sh/hook: test` annotation
- `image: busybox:1.36`
- Runs `wget -qO- http://<service>:<port>/healthz` and grep for `"status":"ok"`.
- Pod-level securityContext mirrors the main deployment (nonroot, readOnlyRootFilesystem, drop caps).

### `NOTES.txt`
- Print port-forward command, dashboard URL, and a notice about the `helm.sh/resource-policy: keep` annotation on the PVC.

## CI workflow `.github/workflows/helm-lint.yml`
- Triggers: `pull_request` on paths `deploy/helm/**`, `.github/workflows/helm-lint.yml`; `push` to `main` on the same paths.
- Steps:
  1. `actions/checkout@v6`
  2. `azure/setup-helm@v4`
  3. `helm dependency build deploy/helm/dicode` — no-op for now but future-proof
  4. `helm lint --strict deploy/helm/dicode`
  5. `helm template smoke deploy/helm/dicode > /tmp/manifests.yaml` (default values)
  6. `helm template smoke deploy/helm/dicode --set ingress.enabled=true --set secret.create=true --set secret.values.DICODE_PASSPHRASE=demo --set 'config=server:\n  port: 8080' > /tmp/manifests-extras.yaml` (exercise conditional templates)
  7. `azure/setup-kubectl@v4` then `kubectl apply --dry-run=client -f /tmp/manifests.yaml` and likewise for the extras render — catches bad manifests without a cluster

## Docs
Append a `### Kubernetes` subsection to `docs/concepts/deployment.md`'s
existing "Docker" → "Configuration reference" flow:

```markdown
### Kubernetes (Helm)

```bash
git clone https://github.com/dicode-ayo/dicode-core
helm install dicode ./dicode-core/deploy/helm/dicode \
  --create-namespace --namespace dicode \
  --set secret.create=true \
  --set secret.values.ANTHROPIC_API_KEY=$ANTHROPIC_API_KEY
```

(Ships `docs/concepts/deployment.md` because the repo doesn't have a
`docs-src/` tree; the issue's reference was aspirational.)

## README.md (chart-local)
- Quickstart `helm install`
- Parameter table (every value in `values.yaml`, with default + description)
- Persistence + cleanup section (`helm uninstall` does NOT remove the PVC)
- Custom config example (mounting `dicode.yaml`)
- Secrets example (passphrase + AI key)
- Ingress example
- Limitations: replicaCount > 1 not yet supported

## Validation strategy

This worktree's environment cannot execute `helm` or `kubectl` directly,
so the chart-author validation reduces to:

1. Render templates by hand into syntactically-valid YAML using the
   structure verified against existing community charts (bitnami,
   prometheus-community) — they are the de-facto helm-3 idiom.
2. Lint the YAML *shape* with Python's `yaml.safe_load_all` (every doc
   parses) — catches indentation bugs.
3. Validate `values.yaml` against `values.schema.json` with Python's
   `jsonschema` — catches schema drift.
4. The actual `helm lint --strict` and `kubectl apply --dry-run`
   contracts are then enforced by the new `helm-lint.yml` workflow on
   every PR push, so CI is the canonical gate.

## Out of scope / follow-ups

Will file as separate GH issues after PR is open:
- Leader election in `pkg/cluster/lease.go` so `replicaCount > 1` is safe
- `chart-releaser-action` publishing to `dicode-ayo.github.io/charts`
- Cosign signing of the chart bundle
- NetworkPolicy templates (default-deny + allow-list)
- `kind`/`k3d` smoke test in CI (install + `helm test` + dashboard probe)
- artifacthub.io listing
- Postgres support (#216, already exists)
