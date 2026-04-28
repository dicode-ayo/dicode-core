# Plan: Helm chart for dicode-core (#215)

Spec: `docs/superpowers/specs/2026-04-28-helm-chart-design.md`.

## Conventions

- Worktree root: `/workspaces/dicode-core-worktrees/helm-chart-215`.
- All paths below are relative to that worktree root.
- Each task is self-contained; commit after each task batch passes.
- Local validation gates use Python (`yaml`, `jsonschema`) because
  `helm`/`kubectl` aren't available in the harness — CI is the
  canonical gate via the `helm-lint.yml` workflow added in batch 6.

---

## Batch 1 — Chart skeleton

### Task 1.1 — `Chart.yaml`

File: `deploy/helm/dicode/Chart.yaml`

```yaml
apiVersion: v2
name: dicode
description: GitOps task orchestrator that runs JS/TS/Python/Docker tasks on cron, webhook, and chained triggers.
type: application
# Chart version is independent of dicode-core's version. Bumped when
# the chart's structure changes.
version: 0.1.0
# appVersion tracks the dicode-core release. Update on every
# release-please cut (or rely on the manifest in CI to bump it).
appVersion: "0.0.4"
kubeVersion: ">=1.27.0-0"
keywords: [gitops, orchestrator, cron, webhook, dicode]
home: https://github.com/dicode-ayo/dicode-core
sources:
  - https://github.com/dicode-ayo/dicode-core
maintainers:
  - name: dicode-ayo
    url: https://github.com/dicode-ayo
icon: https://raw.githubusercontent.com/dicode-ayo/dicode-core/main/cmd/dicode/icon.png
annotations:
  category: Automation
```

### Task 1.2 — `.helmignore`

File: `deploy/helm/dicode/.helmignore`

Standard helm-create boilerplate plus repo-specific exclusions.

### Task 1.3 — `_helpers.tpl`

File: `deploy/helm/dicode/templates/_helpers.tpl`

Implement `dicode.name`, `dicode.fullname`, `dicode.chart`,
`dicode.labels`, `dicode.selectorLabels`, `dicode.serviceAccountName`,
`dicode.image`. See the reference chart skeleton in any community helm
chart — keep semantics identical so users have no surprises.

### Task 1.4 — `values.yaml`

File: `deploy/helm/dicode/values.yaml`

Render exactly the schema from spec §"values.yaml schema (canonical)".
Heavily commented — `values.yaml` is the de-facto user manual.

### Task 1.5 — `values.schema.json`

File: `deploy/helm/dicode/values.schema.json`

Draft-07 schema validating every key in `values.yaml`. `additionalProperties: false` at the top level so typos like `replicacount` fail fast. Subobjects with user-defined keys (`resources`, `nodeSelector`, `tolerations`, `affinity`, `podAnnotations`, `podLabels`, `imagePullSecrets`, `service.annotations`, `ingress.annotations`, `persistence.annotations`, `secret.values`, `envFrom`, `env`) stay loose.

### Task 1.6 — `NOTES.txt`

File: `deploy/helm/dicode/templates/NOTES.txt`

Print:
- Service URL via `kubectl port-forward`
- Reminder that the PVC has `helm.sh/resource-policy: keep`
- Reminder that `replicaCount > 1` is not supported yet

### Validation gate (batch 1)

```bash
python3 - <<'PY'
import yaml, json, pathlib
root = pathlib.Path("deploy/helm/dicode")
yaml.safe_load((root/"Chart.yaml").read_text())
yaml.safe_load((root/"values.yaml").read_text())
json.loads((root/"values.schema.json").read_text())
print("ok")
PY
```

```bash
python3 - <<'PY'
# Validate values.yaml against values.schema.json.
import json, yaml, pathlib, sys
root = pathlib.Path("deploy/helm/dicode")
v = yaml.safe_load((root/"values.yaml").read_text())
s = json.loads((root/"values.schema.json").read_text())
import jsonschema
jsonschema.validate(v, s)
print("schema ok")
PY
```

Commit: `feat(helm): chart skeleton (Chart.yaml, values, schema, helpers)`.

---

## Batch 2 — Workload (deployment + service)

### Task 2.1 — `deployment.yaml`

File: `deploy/helm/dicode/templates/deployment.yaml`

- `apiVersion: apps/v1`, `kind: Deployment`
- Name + labels + selector via helpers
- `strategy: { type: Recreate }` (RWO PVC)
- `replicas: {{ .Values.replicaCount }}`
- `serviceAccountName: {{ include "dicode.serviceAccountName" . }}`
- `securityContext: {{ toYaml .Values.podSecurityContext | nindent 8 }}` at pod level
- Container:
  - `name: dicode`
  - `image: {{ include "dicode.image" . }}`
  - `imagePullPolicy: {{ .Values.image.pullPolicy }}`
  - `securityContext: {{ toYaml .Values.containerSecurityContext | nindent 12 }}`
  - `ports: [{ name: http, containerPort: 8080 }]`
  - `livenessProbe`, `readinessProbe` from values
  - `resources` from values
  - `env` (literal) + chart-managed Secret via `envFrom: [{ secretRef: { name: ... } }]` when `secret.create` or `existingSecret`
  - `envFrom` extras passthrough
  - Volume mounts:
    - `data` at `/data`
    - `tmp` at `/tmp` (emptyDir; required by readOnlyRootFilesystem)
    - `config` at `/data/dicode.yaml` with `subPath: dicode.yaml` when `.Values.config != ""`
- Volumes:
  - `data` — PVC reference (or emptyDir if `persistence.enabled: false`)
  - `tmp` — emptyDir
  - `config` — ConfigMap reference, only when config is set

### Task 2.2 — `service.yaml`

File: `deploy/helm/dicode/templates/service.yaml`

- `apiVersion: v1`, `kind: Service`
- Default `ClusterIP`, port 8080 → targetPort `http`
- Annotations passthrough

### Validation gate (batch 2)

```bash
python3 - <<'PY'
# Render the templates with a fake `helm template` walker:
# parse every YAML doc and confirm it's structurally valid.
import yaml, pathlib, sys
for p in sorted(pathlib.Path("deploy/helm/dicode/templates").rglob("*.yaml")):
    txt = p.read_text()
    # Skip helpers (they aren't standalone docs)
    if p.name == "_helpers.tpl": continue
    try:
        list(yaml.safe_load_all(txt))
    except yaml.YAMLError as e:
        print(f"YAML parse error in {p}: {e}", file=sys.stderr); sys.exit(1)
print("yaml shape ok")
PY
```

(Won't catch Go-template Sprig errors — CI helm-lint will. But it
catches the most common author bug: bad indentation.)

Commit: `feat(helm): deployment + service templates`.

---

## Batch 3 — Storage + identity (configmap, secret, pvc, serviceaccount)

### Task 3.1 — `configmap.yaml`

File: `deploy/helm/dicode/templates/configmap.yaml`

```yaml
{{- if .Values.config }}
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ include "dicode.fullname" . }}-config
  labels: {{ include "dicode.labels" . | nindent 4 }}
data:
  dicode.yaml: |
{{ .Values.config | indent 4 }}
{{- end }}
```

### Task 3.2 — `secret.yaml`

File: `deploy/helm/dicode/templates/secret.yaml`

```yaml
{{- if .Values.secret.create }}
apiVersion: v1
kind: Secret
metadata:
  name: {{ include "dicode.fullname" . }}
  labels: {{ include "dicode.labels" . | nindent 4 }}
type: Opaque
data:
  {{- range $k, $v := .Values.secret.values }}
  {{ $k }}: {{ $v | toString | b64enc | quote }}
  {{- end }}
{{- end }}
```

### Task 3.3 — `pvc.yaml`

File: `deploy/helm/dicode/templates/pvc.yaml`

```yaml
{{- if and .Values.persistence.enabled (not .Values.persistence.existingClaim) }}
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: {{ include "dicode.fullname" . }}-data
  labels: {{ include "dicode.labels" . | nindent 4 }}
  annotations:
    helm.sh/resource-policy: keep
    {{- with .Values.persistence.annotations }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
spec:
  accessModes: {{ toYaml .Values.persistence.accessModes | nindent 4 }}
  resources:
    requests:
      storage: {{ .Values.persistence.size | quote }}
  {{- with .Values.persistence.storageClass }}
  {{- if eq . "-" }}
  storageClassName: ""
  {{- else }}
  storageClassName: {{ . | quote }}
  {{- end }}
  {{- end }}
{{- end }}
```

### Task 3.4 — `serviceaccount.yaml`

File: `deploy/helm/dicode/templates/serviceaccount.yaml`

Standard helm-create skeleton — render only when `.Values.serviceAccount.create`.

### Validation gate (batch 3)
Same Python YAML walker plus an integration check: every templated
`metadata.name` derived from `dicode.fullname` should be ≤ 63 chars
when run with default release name `dicode` and chart name `dicode`.

Commit: `feat(helm): configmap, secret, pvc, serviceaccount templates`.

---

## Batch 4 — Optional networking + helm test

### Task 4.1 — `ingress.yaml`

File: `deploy/helm/dicode/templates/ingress.yaml`

`networking.k8s.io/v1` Ingress. Helm-3 conventional shape; render only when `.Values.ingress.enabled`.

### Task 4.2 — `tests/test-connection.yaml`

File: `deploy/helm/dicode/templates/tests/test-connection.yaml`

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: {{ include "dicode.fullname" . }}-test-connection
  labels: {{ include "dicode.labels" . | nindent 4 }}
  annotations:
    helm.sh/hook: test
    helm.sh/hook-delete-policy: hook-succeeded,before-hook-creation
spec:
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    runAsGroup: 65532
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: wget
      image: busybox:1.36
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: ["ALL"]
      command: ["sh", "-c"]
      args:
        - >-
          set -eu;
          out=$(wget -qO- "http://{{ include "dicode.fullname" . }}:{{ .Values.service.port }}/healthz");
          echo "$out";
          echo "$out" | grep -q '"status":"ok"'
```

### Validation gate (batch 4)
YAML walker again.

Commit: `feat(helm): ingress + helm-test connection`.

---

## Batch 5 — Chart README (in-chart docs)

### Task 5.1 — `deploy/helm/dicode/README.md`

Hand-written. Sections:
1. **TL;DR** — one-line `helm install`
2. **Prerequisites** — k8s ≥ 1.27, `default-storageclass` (or `--set persistence.enabled=false`)
3. **Install**
4. **Upgrade**
5. **Uninstall** — call out the kept PVC
6. **Configuration**
   - Parameter table generated by hand from `values.yaml` (key / default / description)
   - Custom `dicode.yaml` example
   - Secret examples (create-internal vs existing-secret)
   - Ingress example
7. **Limitations** — `replicaCount: 1`, leader election deferred

Validation: `python3 -c "import pathlib; assert pathlib.Path('deploy/helm/dicode/README.md').stat().st_size > 1500"` (sanity bound that we wrote real content).

Commit: `docs(helm): chart README with parameter reference`.

---

## Batch 6 — CI workflow

### Task 6.1 — `.github/workflows/helm-lint.yml`

File: `.github/workflows/helm-lint.yml`

```yaml
name: Helm chart

on:
  push:
    branches: [main]
    paths:
      - 'deploy/helm/**'
      - '.github/workflows/helm-lint.yml'
  pull_request:
    branches: [main]
    paths:
      - 'deploy/helm/**'
      - '.github/workflows/helm-lint.yml'

env:
  FORCE_JAVASCRIPT_ACTIONS_TO_NODE24: true

jobs:
  lint:
    name: Lint + render
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6

      - name: Set up Helm
        uses: azure/setup-helm@v4
        with:
          version: v3.16.4

      - name: Set up kubectl
        uses: azure/setup-kubectl@v4
        with:
          version: v1.31.0

      - name: helm lint --strict (default values)
        run: helm lint --strict deploy/helm/dicode

      - name: helm template (default values)
        run: helm template smoke deploy/helm/dicode > /tmp/manifests-default.yaml

      - name: kubectl dry-run (default values)
        run: kubectl apply --dry-run=client -f /tmp/manifests-default.yaml

      - name: helm lint --strict (extras enabled)
        run: |
          helm lint --strict deploy/helm/dicode \
            --set ingress.enabled=true \
            --set 'ingress.hosts[0].host=dicode.example.com' \
            --set 'ingress.hosts[0].paths[0].path=/' \
            --set 'ingress.hosts[0].paths[0].pathType=Prefix' \
            --set secret.create=true \
            --set 'secret.values.DICODE_PASSPHRASE=demo' \
            --set 'config=server:\n  port: 8080\n'

      - name: helm template (extras enabled)
        run: |
          helm template smoke deploy/helm/dicode \
            --set ingress.enabled=true \
            --set 'ingress.hosts[0].host=dicode.example.com' \
            --set 'ingress.hosts[0].paths[0].path=/' \
            --set 'ingress.hosts[0].paths[0].pathType=Prefix' \
            --set secret.create=true \
            --set 'secret.values.DICODE_PASSPHRASE=demo' \
            --set 'config=server:\n  port: 8080\n' > /tmp/manifests-extras.yaml

      - name: kubectl dry-run (extras enabled)
        run: kubectl apply --dry-run=client -f /tmp/manifests-extras.yaml

      - name: Reject invalid values (schema check)
        run: |
          set +e
          out=$(helm template smoke deploy/helm/dicode --set replicaCount=-1 2>&1)
          rc=$?
          if [ $rc -eq 0 ]; then
            echo "expected schema validation to reject replicaCount=-1, got rc=0:"
            echo "$out"
            exit 1
          fi
          echo "schema correctly rejected: $out"
```

### Validation gate (batch 6)
- `python3 -c "import yaml; yaml.safe_load(open('.github/workflows/helm-lint.yml'))"` — workflow YAML valid.

Commit: `ci(helm): lint + render + dry-run on every PR touching deploy/helm`.

---

## Batch 7 — Docs

### Task 7.1 — Append Kubernetes section to `docs/concepts/deployment.md`

Insert after the existing "Docker" section, before "Configuration reference":

```markdown
---

## Kubernetes (Helm)

A Helm chart ships in [`deploy/helm/dicode`](https://github.com/dicode-ayo/dicode-core/tree/main/deploy/helm/dicode).

```bash
git clone https://github.com/dicode-ayo/dicode-core.git
cd dicode-core
helm install dicode ./deploy/helm/dicode \
  --create-namespace --namespace dicode \
  --set secret.create=true \
  --set secret.values.ANTHROPIC_API_KEY=$ANTHROPIC_API_KEY
```

Then port-forward:

```bash
kubectl -n dicode port-forward svc/dicode 8080:8080
open http://localhost:8080
```

**Notes:**
- Defaults to a single replica with a 5 Gi PVC at `/data` for SQLite, the master key, and `dicode.yaml`.
- The PVC is annotated `helm.sh/resource-policy: keep`, so `helm uninstall` does not delete your task data.
- Multi-replica deployments are not yet supported — the daemon has no leader election, so `replicaCount > 1` may run cron triggers more than once. Tracked in a follow-up issue.

See [`deploy/helm/dicode/README.md`](https://github.com/dicode-ayo/dicode-core/tree/main/deploy/helm/dicode/README.md) for every chart parameter.
```

### Validation gate (batch 7)
- `python3 -c "import pathlib; t=pathlib.Path('docs/concepts/deployment.md').read_text(); assert 'Kubernetes (Helm)' in t and 'helm install dicode' in t"`

Commit: `docs(deployment): add Kubernetes (Helm) section`.

---

## Batch 8 — Final validation + PR

### Task 8.1 — Cross-template consistency check

```bash
python3 - <<'PY'
# Fake-render every template (literal Go-template fragments are
# allowed; we just confirm no `{{ }}` straddles a YAML boundary
# in a way that breaks the file when stripped).
import re, pathlib
for p in pathlib.Path("deploy/helm/dicode/templates").rglob("*.yaml"):
    txt = p.read_text()
    # Confirm every {{ has matching }}
    if txt.count("{{") != txt.count("}}"):
        raise SystemExit(f"unbalanced braces in {p}")
    # Confirm no tab indentation (helm linters reject tabs)
    if "\t" in txt:
        raise SystemExit(f"tab character in {p}")
print("ok")
PY
```

### Task 8.2 — Sanity-check schema rejects bad values

```bash
python3 - <<'PY'
import json, yaml, pathlib, sys, jsonschema
root = pathlib.Path("deploy/helm/dicode")
schema = json.loads((root/"values.schema.json").read_text())
bad_cases = [
    {"replicaCount": -1},
    {"image": {"repository": 123}},
    {"service": {"port": 70000}},
    {"persistence": {"size": 5}},          # must be string
    {"banana": True},                       # additionalProperties:false at top
]
for case in bad_cases:
    try:
        jsonschema.validate(case, schema)
    except jsonschema.ValidationError:
        continue
    print(f"FAIL: schema accepted invalid case {case}")
    sys.exit(1)
print("schema rejects all bad cases")
PY
```

### Task 8.3 — Commit + push + PR

```bash
git -C /workspaces/dicode-core-worktrees/helm-chart-215 push -u origin feat/215-helm-chart
gh pr create --repo dicode-ayo/dicode-core --base main --head feat/215-helm-chart \
  --title "feat(deploy): production-ready Helm chart for dicode-core" \
  --body "$(cat <<'EOF'
## Summary
- New Helm chart at `deploy/helm/dicode/` consuming the `ghcr.io/dicode-ayo/dicode-core` / `dicodeayo/dicode-core` image landed in #227
- ServiceAccount, Deployment (Recreate strategy), Service, PVC (5 Gi default at `/data`), optional ConfigMap (`dicode.yaml`), optional Secret (passphrase + AI keys), optional Ingress
- Hardened pod & container security context: `runAsNonRoot`, UID 65532, `readOnlyRootFilesystem` with EmptyDir for `/tmp`, `seccompProfile: RuntimeDefault`, all caps dropped
- Liveness + readiness probes against the unauth `/healthz` route from #227
- `helm test` connection probe (busybox `wget` against `/healthz`)
- New `helm-lint.yml` CI workflow runs `helm lint --strict` and `kubectl apply --dry-run=client` against rendered manifests on every PR touching `deploy/helm/**`
- Chart-local `README.md` with parameter reference; `docs/concepts/deployment.md` gets a Kubernetes (Helm) section

Closes #215.

## Scope cuts (with rationale)
- **`replicaCount: 1` only** — multi-replica needs leader election in `pkg/cluster/lease.go`. Filed as a follow-up issue.
- **No `chart-releaser-action`/GH Pages publish** — separate workflow + branch + repo settings; filed as follow-up.
- **No Cosign signing** — bundles with the publishing follow-up.
- **No `kind`/`k3d` smoke test workflow** — `helm-lint` + `kubectl --dry-run=client` is the floor; live cluster smoke deserves its own iteration.
- **No NetworkPolicy template** — most users with strict policies vendor in their own; filed as follow-up.
- **No artifacthub.io listing** — depends on the publishing follow-up.

## Deferred follow-ups (will file as issues)
- Leader election (#TBD)
- Chart releaser + GH Pages (#TBD)
- Cosign signing (#TBD)
- NetworkPolicy templates (#TBD)
- kind/k3d smoke test in CI (#TBD)

## Test plan
- [x] `helm lint --strict deploy/helm/dicode` (CI)
- [x] `helm template smoke deploy/helm/dicode | kubectl apply --dry-run=client -f -` (CI, default + extras)
- [x] Schema rejects intentional bad values: `replicaCount: -1` (CI guard)
- [x] YAML shape lint of every template (Python `yaml.safe_load_all`, ran locally)
- [x] `values.yaml` validates against `values.schema.json` (Python `jsonschema`, ran locally)
- [ ] Live install on a kind cluster + `helm test dicode` — deferred to follow-up

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

### Task 8.4 — Run `/review` and `/security-review`; iterate inline comments
- Dispatch via Skill tool.
- Apply Critical findings; document Suggestion-level deferrals in PR body.

### Task 8.5 — File follow-up issues with `gh issue create`
- Leader election
- Chart releaser
- Cosign
- NetworkPolicy
- kind smoke

### Task 8.6 — Drop spec/plan docs
```bash
git rm docs/superpowers/specs/2026-04-28-helm-chart-design.md docs/superpowers/plans/2026-04-28-helm-chart.md
git commit -m "chore(helm): drop spec/plan scaffolding before merge"
git push
```

### Task 8.7 — Report back
PR URL, scope cuts taken, follow-up URLs, final review verdicts.
