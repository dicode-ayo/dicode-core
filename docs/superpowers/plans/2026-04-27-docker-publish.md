# Dockerfile + Docker Hub/GHCR Publish — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a multi-stage, multi-arch Dockerfile to dicode-core and dual-publish images to Docker Hub + GHCR on every release-please-driven tag.

**Architecture:** Multi-stage Dockerfile (`golang:1.25-alpine` → `gcr.io/distroless/static-debian12:nonroot`), buildx cross-compilation (amd64 + arm64), tag-only publish via a new `publish-docker` job in the existing `release-please.yml`. A new `/healthz` chi route on the webui supports CI smoke tests and downstream Helm probes.

**Tech Stack:** Go 1.25, chi router, Docker buildx, `docker/build-push-action@v7`, `docker/metadata-action@v6`, `softprops/action-gh-release@v3`, distroless base image.

**Spec:** [`docs/superpowers/specs/2026-04-27-docker-publish-design.md`](../specs/2026-04-27-docker-publish-design.md)

**Worktree:** `/workspaces/dicode-core-worktrees/docker-publish/` on branch `feat/docker-publish`. All commands assume `cwd` is that directory unless otherwise noted.

---

## File Structure

| File | Status | Responsibility |
|---|---|---|
| `pkg/webui/server.go` | Modify | Register `/healthz` chi route before auth; declare `Version` package var |
| `pkg/webui/healthz_test.go` | Create | Unit-test `/healthz`: 200, JSON shape, auth bypass, version reporting |
| `pkg/daemon/daemon.go` | Modify | Set `webui.Version = version` before calling `webui.New(...)` |
| `Dockerfile` | Create | Multi-stage cross-compiled Go build → distroless runtime |
| `.dockerignore` | Create | Keep build context lean |
| `.github/workflows/release-please.yml` | Modify | Add `publish-docker` job parallel to existing `goreleaser` job |
| `README.md` | Modify | Add a Docker install section |
| `docs-src/getting-started/index.md` | Modify | Add a Docker quick-start subsection |

---

## Task 1: Add `Version` package var and `/healthz` chi route (TDD)

**Files:**
- Test: `pkg/webui/healthz_test.go` (create)
- Modify: `pkg/webui/server.go`

### - [ ] Step 1.1: Write failing healthz test

Create `pkg/webui/healthz_test.go`:

```go
package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/trigger"
	"go.uber.org/zap"
)

// newHealthServer builds a Server with auth enabled, since the strongest test
// of /healthz is that it bypasses auth.
func newHealthServer(t *testing.T) *Server {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	cfg := &config.Config{
		Server: config.ServerConfig{
			Port:   8080,
			Auth:   true,
			Secret: "test-passphrase",
		},
	}
	srv, err := New(8080, reg, eng, cfg, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())
	if err != nil {
		t.Fatalf("webui.New: %v", err)
	}
	return srv
}

func TestHealthzReturns200WithJSON(t *testing.T) {
	srv := newHealthServer(t)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type: got %q, want application/json", ct)
	}

	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status field: got %q, want %q", body["status"], "ok")
	}
	if _, ok := body["version"]; !ok {
		t.Errorf("response missing version field: %v", body)
	}
}

func TestHealthzBypassesAuth(t *testing.T) {
	// With auth enabled, an unauthenticated request to /healthz must succeed.
	// Any 4xx/5xx (especially 401/302-to-login) means the route is gated.
	srv := newHealthServer(t)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("auth-enabled /healthz: got %d, want 200 (route must bypass auth)", w.Code)
	}
}

func TestHealthzReportsConfiguredVersion(t *testing.T) {
	prev := Version
	Version = "v9.9.9-test"
	t.Cleanup(func() { Version = prev })

	srv := newHealthServer(t)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	var body map[string]string
	_ = json.NewDecoder(w.Body).Decode(&body)
	if body["version"] != "v9.9.9-test" {
		t.Errorf("version: got %q, want %q", body["version"], "v9.9.9-test")
	}
}
```

### - [ ] Step 1.2: Run test to verify it fails

```sh
cd /workspaces/dicode-core-worktrees/docker-publish
go test ./pkg/webui/ -run TestHealthz -v -timeout 30s
```

Expected: build error or test failure — `Version` symbol not defined and `/healthz` returns 404 / redirect-to-login.

### - [ ] Step 1.3: Add `Version` package var and `/healthz` route

In `pkg/webui/server.go`:

**Add a package-level `Version` var** near the top of the file, after the existing package-level vars/imports:

```go
// Version is the build-stamped version reported by GET /healthz. Set by
// pkg/daemon before calling webui.New(). Defaults to "dev" when unset.
var Version = "dev"
```

**Register `/healthz` on the chi router** — locate the public-route block in `Handler()` (around line 386, alongside `r.Post("/api/auth/refresh", ...)`). Add the healthz registration immediately after the middleware stack and before the auth-protected group at line 432:

```go
// /healthz: unauthenticated liveness probe. Used by Docker smoke tests,
// Kubernetes liveness/readiness probes, and uptime monitors. Must remain
// outside the auth-required group.
r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"version": Version,
	})
})
```

If `encoding/json` isn't already imported in `server.go`, add it.

### - [ ] Step 1.4: Run tests to verify they pass

```sh
go test ./pkg/webui/ -run TestHealthz -v -timeout 30s
```

Expected: all three `TestHealthz*` tests PASS.

### - [ ] Step 1.5: Run full webui test suite to verify no regressions

```sh
go test ./pkg/webui/... -timeout 60s
```

Expected: PASS — confirms /healthz registration didn't break route ordering or existing auth flows.

### - [ ] Step 1.6: Commit

```sh
git add pkg/webui/server.go pkg/webui/healthz_test.go
git commit -m "$(cat <<'EOF'
feat(webui): add /healthz liveness probe

Unauthenticated route returning JSON {status: "ok", version: <x>}.
Required by the Docker smoke test in CI and by Helm liveness/readiness
probes in the upcoming chart.

Refs #210
EOF
)"
```

---

## Task 2: Plumb version from daemon to webui.Version

**Files:**
- Modify: `pkg/daemon/daemon.go`

### - [ ] Step 2.1: Set `webui.Version` before `webui.New(...)` call

Locate the `webui.New(port, ...)` call (around line 195 in `pkg/daemon/daemon.go`) and add a single line before it:

```go
webui.Version = version
srv, err := webui.New(port, reg, eng, cfg, configPath, localSecrets, rec, sourceMgr, dataDir, logBroadcaster, log, database, gateway)
```

The `version` local variable is already passed into `daemon.run(...)` as a parameter (line 110); no further plumbing needed. `webui.Version` defaults to `"dev"` when daemon code paths don't set it (e.g., test fixtures), which is fine.

### - [ ] Step 2.2: Run daemon + webui tests

```sh
go test ./pkg/daemon/... ./pkg/webui/... -timeout 60s
```

Expected: PASS.

### - [ ] Step 2.3: Build the binary to verify the link is intact

```sh
make build
./dicode version
```

Expected: prints `dev` (no `-X` ldflag was passed) — confirms the binary still compiles and runs.

### - [ ] Step 2.4: Commit

```sh
git add pkg/daemon/daemon.go
git commit -m "$(cat <<'EOF'
feat(daemon): plumb build version into webui.Version

Sets the package-level webui.Version so /healthz reports the same
version string the daemon log emits at startup. No new parameters;
reuses the existing `version` arg threaded through daemon.run.

Refs #210
EOF
)"
```

---

## Task 3: Add `.dockerignore`

**Files:**
- Create: `.dockerignore`

### - [ ] Step 3.1: Create the file

```
.git
.github
.worktrees
.claude
.devcontainer
.remember
.vscode
docs
docs-src
node_modules
coverage
dist
**/*.test
**/*.out
dicode
playwright.config.ts
tests/e2e
*.md
!README.md
```

(Note: `*.md` excludes the giant CHANGELOG/dyad-issues-triage files; we keep `README.md` because some operators inspect images via `docker run --rm <image> cat /README.md`-style flows. We don't actually copy README into the image, but keeping it whitelisted is harmless and protects against future Dockerfile edits.)

### - [ ] Step 3.2: Verify build context size shrinks

```sh
docker build --no-cache -f - . <<'EOF' || true
FROM alpine
COPY . /ctx
RUN du -sh /ctx
EOF
```

Expected: prints a context size in the low single-digit MB range. If the output shows hundreds of MB, the dockerignore is missing something — investigate (likely `.git` or `pkg/.../testdata`).

### - [ ] Step 3.3: Commit

```sh
git add .dockerignore
git commit -m "$(cat <<'EOF'
build: add .dockerignore

Trim the build context to source + go.{mod,sum} + Makefile so docker
build doesn't ship .git, docs, worktrees, or e2e fixtures to the daemon.

Refs #210
EOF
)"
```

---

## Task 4: Add `Dockerfile`

**Files:**
- Create: `Dockerfile`

### - [ ] Step 4.1: Create the Dockerfile

```dockerfile
# syntax=docker/dockerfile:1.7

# Build args (overridable from CI):
#   GO_VERSION     — keep in sync with go.mod's `go` directive
#   ALPINE_VERSION — alpine base for the builder stage
#   VERSION        — semver string stamped into the binary via -ldflags
ARG GO_VERSION=1.25
ARG ALPINE_VERSION=3.21   # golang:1.25 ships alpine3.21+ only — 3.20 tag does not exist

# --- Build stage ----------------------------------------------------------
# Run on the host's native arch ($BUILDPLATFORM); cross-compile via
# GOOS/GOARCH so the Go compiler is never QEMU-emulated.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src

# Cache module downloads on a separate layer
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/dicode ./cmd/dicode

# --- Runtime stage --------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source="https://github.com/dicode-ayo/dicode-core"
LABEL org.opencontainers.image.description="dicode — GitOps task orchestrator"
LABEL org.opencontainers.image.licenses="Apache-2.0"

COPY --from=build /out/dicode /usr/local/bin/dicode

# /data is the conventional mount point for SQLite state; users override
# `--data-dir` or set DICODE_DATA_DIR to relocate.
VOLUME ["/data"]

EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/dicode"]
CMD ["daemon"]
```

### - [ ] Step 4.2: Local build (single-arch, fast iteration)

```sh
docker build -t dicode-core:local --build-arg VERSION=v0.0.0-local .
```

Expected: build succeeds; final image listed by `docker images dicode-core:local` is < 60 MB.

### - [ ] Step 4.3: Local smoke test

```sh
docker rm -f dicode-smoke 2>/dev/null || true
docker run -d --name dicode-smoke -p 18080:8080 dicode-core:local

for i in $(seq 1 30); do
  if curl -fsS http://localhost:18080/healthz; then
    echo "✓ healthz OK after ${i}s"
    break
  fi
  sleep 1
done

curl -fsS http://localhost:18080/healthz | python3 -m json.tool
docker logs dicode-smoke | tail -20
docker rm -f dicode-smoke
```

Expected: `/healthz` returns `{"status":"ok","version":"v0.0.0-local"}` within 30s; logs show "dicode daemon starting" and "webui listening".

### - [ ] Step 4.4: Multi-arch local sanity check (optional but recommended)

```sh
docker buildx create --use --name dicode-builder 2>/dev/null || docker buildx use dicode-builder
docker buildx build --platform linux/amd64,linux/arm64 -t dicode-core:multiarch --build-arg VERSION=v0.0.0-local .
```

Expected: build succeeds for both platforms. (Buildx will not push without `--push`; this just verifies the cross-compile path.)

### - [ ] Step 4.5: Commit

```sh
git add Dockerfile
git commit -m "$(cat <<'EOF'
build: multi-stage, multi-arch Dockerfile

Cross-compiles via $BUILDPLATFORM + GOOS/GOARCH (no QEMU emulation
of the Go toolchain). Distroless runtime, runs as nonroot, ~35 MB.
ENTRYPOINT is the dicode binary; default CMD is `daemon`.

Refs #210
EOF
)"
```

---

## Task 5: Add `publish-docker` job to `release-please.yml`

**Files:**
- Modify: `.github/workflows/release-please.yml`

### - [ ] Step 5.1: Read the current workflow

```sh
cat .github/workflows/release-please.yml
```

Expected: confirms the `release-please` and `goreleaser` jobs exist with `release_created`/`tag_name` outputs already exposed by the `release-please` job.

### - [ ] Step 5.2: Add the `publish-docker` job

Append the following job under the existing two jobs in `.github/workflows/release-please.yml` (sibling to `goreleaser`, no inter-job ordering required):

```yaml
  publish-docker:
    name: Publish Docker image
    needs: release-please
    if: ${{ needs.release-please.outputs.release_created }}
    runs-on: ubuntu-latest
    permissions:
      contents: write   # softprops/action-gh-release append
      packages: write   # ghcr.io push
    steps:
      - uses: actions/checkout@v6

      - uses: docker/setup-qemu-action@v3
      - uses: docker/setup-buildx-action@v3

      - name: Log in to Docker Hub
        uses: docker/login-action@v4
        with:
          username: ${{ secrets.DOCKERHUB_USERNAME }}
          password: ${{ secrets.DOCKERHUB_TOKEN }}

      - name: Log in to GitHub Container Registry
        uses: docker/login-action@v4
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}

      # metadata-action fans the same tag set across both images (Docker Hub
      # primary + ghcr.io secondary) so a single build-push step pushes both.
      # `value=${{ needs.release-please.outputs.tag_name }}` makes the version
      # source-of-truth the release-please tag, not push-event metadata.
      - name: Extract image tags
        id: meta
        uses: docker/metadata-action@v6
        with:
          images: |
            docker.io/dicodeayo/dicode-core
            ghcr.io/dicode-ayo/dicode-core
          tags: |
            type=semver,pattern={{version}},value=${{ needs.release-please.outputs.tag_name }}
            type=semver,pattern={{major}}.{{minor}},value=${{ needs.release-please.outputs.tag_name }}
            type=semver,pattern={{major}},value=${{ needs.release-please.outputs.tag_name }}
            type=raw,value=latest

      - name: Build and push (multi-arch)
        id: build
        uses: docker/build-push-action@v7
        with:
          context: .
          platforms: linux/amd64,linux/arm64
          push: true
          tags: ${{ steps.meta.outputs.tags }}
          labels: ${{ steps.meta.outputs.labels }}
          build-args: |
            VERSION=${{ steps.meta.outputs.version }}
          cache-from: type=gha
          cache-to: type=gha,mode=max

      - name: Smoke test pushed image (/healthz)
        run: |
          IMAGE=ghcr.io/dicode-ayo/dicode-core:${{ steps.meta.outputs.version }}
          docker run -d --name smoke -p 8080:8080 "$IMAGE"
          for i in $(seq 1 30); do
            if curl -fsS http://localhost:8080/healthz; then
              echo "✓ healthz OK after ${i}s"
              docker rm -f smoke
              exit 0
            fi
            sleep 1
          done
          echo "✗ /healthz never returned 200"
          docker logs smoke
          docker rm -f smoke
          exit 1

      # release-please created the GitHub Release at PR-merge time with the
      # curated changelog; goreleaser appended binary-install instructions.
      # We append a Docker-install block on top of those, preserving both.
      - name: Append Docker install instructions to GitHub Release
        uses: softprops/action-gh-release@v3
        with:
          tag_name: ${{ needs.release-please.outputs.tag_name }}
          append_body: true
          body: |

            ## Docker image

            ```
            docker pull dicodeayo/dicode-core:${{ steps.meta.outputs.version }}
            docker run -p 8080:8080 dicodeayo/dicode-core:${{ steps.meta.outputs.version }}
            # or, from GitHub's registry
            docker pull ghcr.io/dicode-ayo/dicode-core:${{ steps.meta.outputs.version }}
            ```

            Multi-arch images are published for `linux/amd64` and `linux/arm64`.
```

### - [ ] Step 5.3: Validate workflow YAML

```sh
# Use Python's yaml because it's the most universally available parser
python3 -c "import yaml; yaml.safe_load(open('.github/workflows/release-please.yml'))" && echo OK
```

Expected: prints `OK`. Any YAML syntax error fails this step.

### - [ ] Step 5.4: Optional — `actionlint` if available

```sh
command -v actionlint >/dev/null && actionlint .github/workflows/release-please.yml || echo "actionlint not installed, skipping"
```

Expected: `actionlint` exits 0, or the message `actionlint not installed, skipping`. Don't fail the task on missing tool.

### - [ ] Step 5.5: Commit

```sh
git add .github/workflows/release-please.yml
git commit -m "$(cat <<'EOF'
ci: dual-publish Docker image on release-please tag

New publish-docker job runs in parallel with goreleaser when
release-please creates a tag. Builds linux/amd64 + linux/arm64,
pushes to docker.io/dicodeayo/dicode-core (primary) and
ghcr.io/dicode-ayo/dicode-core (secondary), smoke-tests /healthz,
and appends pull instructions to the GitHub Release.

Mirrors the relay release.yml publish pattern.

Closes #210
EOF
)"
```

---

## Task 6: Add Docker section to `README.md`

**Files:**
- Modify: `README.md`

### - [ ] Step 6.1: Locate the install section

```sh
grep -n "^## " README.md | head -20
```

Identify the existing install/quickstart heading; the new Docker section goes immediately after it.

### - [ ] Step 6.2: Insert the Docker section

Add this block after the existing install instructions (place it under whatever heading currently introduces installation methods — adjust the heading level if README uses `###` for sub-installs):

```markdown
## Run with Docker

```sh
docker pull dicodeayo/dicode-core:latest
docker run -d --name dicode \
  -p 8080:8080 \
  -v dicode-data:/data \
  dicodeayo/dicode-core:latest
```

Open http://localhost:8080. SQLite state persists in the named volume
`dicode-data`, so you can `docker stop` / `docker start` without losing
runs or registered tasks.

Multi-arch images are published for `linux/amd64` and `linux/arm64` and
mirrored on GHCR: `ghcr.io/dicode-ayo/dicode-core`. Pin to a specific
release: `dicodeayo/dicode-core:0.1.1` (or `:0.1` / `:0`).
```

### - [ ] Step 6.3: Verify Markdown renders

```sh
# Spot-check the inserted block by looking at the surrounding lines
grep -A 20 "Run with Docker" README.md
```

Expected: the block is intact, fenced code blocks balanced, no double-`##` heading conflicts.

### - [ ] Step 6.4: Commit

```sh
git add README.md
git commit -m "docs(readme): add Run with Docker section"
```

---

## Task 7: Add Docker quick-start to `docs-src/getting-started/index.md`

**Files:**
- Modify: `docs-src/getting-started/index.md`

### - [ ] Step 7.1: Inspect current structure

```sh
grep -n "^## \|^### " docs-src/getting-started/index.md | head -20
```

Identify where install methods are currently documented.

### - [ ] Step 7.2: Insert Docker subsection

Add this subsection immediately after whichever install method appears last in the file (preserve the file's existing heading level — match `## ` or `### ` to neighbours):

````markdown
## Docker

The published image runs as a non-root user and exposes the dashboard on
port 8080. SQLite state lives at `/data` inside the container; mount a
volume there to persist runs and task registrations across restarts.

```sh
docker run -d --name dicode \
  -p 8080:8080 \
  -v dicode-data:/data \
  dicodeayo/dicode-core:latest
```

### docker-compose

```yaml
services:
  dicode:
    image: dicodeayo/dicode-core:latest
    restart: unless-stopped
    ports:
      - "8080:8080"
    volumes:
      - dicode-data:/data
    environment:
      # Inject any DICODE_* env vars (e.g. AI provider keys) here.
      # See the secrets reference in the dashboard for required names.

volumes:
  dicode-data:
```

### Image registries

| Registry | Image |
|---|---|
| Docker Hub (primary) | `dicodeayo/dicode-core` |
| GHCR (mirror) | `ghcr.io/dicode-ayo/dicode-core` |

### Tags

- `:latest` — most recent release
- `:X.Y.Z` — exact release (recommended for production)
- `:X.Y` and `:X` — track minor / major lines

Multi-arch (amd64 + arm64) for every published tag.
````

### - [ ] Step 7.3: Verify the file still parses as Markdown

```sh
# Check fenced-block balance: each ``` should match its pair.
awk '/^```/ {n++} END {if (n%2) print "UNBALANCED FENCES"; else print "OK"}' docs-src/getting-started/index.md
```

Expected: prints `OK`.

### - [ ] Step 7.4: Commit

```sh
git add docs-src/getting-started/index.md
git commit -m "docs(getting-started): add Docker quick-start + compose example"
```

---

## Task 8: Push branch and open PR

**Files:** none modified — git/GH operations.

### - [ ] Step 8.1: Push the branch

```sh
git -C /workspaces/dicode-core-worktrees/docker-publish push -u origin feat/docker-publish
```

Expected: branch pushed; remote prints PR-creation link.

### - [ ] Step 8.2: Open the PR

```sh
gh pr create --repo dicode-ayo/dicode-core \
  --title "feat(release): Dockerfile + dual-publish to Docker Hub & GHCR" \
  --body "$(cat <<'EOF'
## Summary
- Multi-stage, multi-arch (amd64 + arm64) `Dockerfile` at repo root using distroless runtime
- New `publish-docker` job in `release-please.yml`, dual-pushes to `dicodeayo/dicode-core` (Docker Hub) and `ghcr.io/dicode-ayo/dicode-core` on every release-please tag
- Adds an unauthenticated `/healthz` route to webui (used by the CI smoke test and the upcoming Helm chart's liveness probe)
- README + getting-started docs cover the Docker install path

Closes #210. Unblocks #215 (Helm chart, separate PR).

Mirrors the relay `release.yml` Docker publish pattern; secrets `DOCKERHUB_USERNAME` / `DOCKERHUB_TOKEN` are already org-level.

## Test plan
- [ ] `go test ./pkg/webui/... -timeout 60s` — passes locally
- [ ] `make build && ./dicode version` — binary still builds + runs
- [ ] `docker build -t dicode-core:local .` — local image builds
- [ ] `docker run -p 18080:8080 dicode-core:local` + `curl /healthz` — returns 200 with version + status
- [ ] `docker buildx build --platform linux/amd64,linux/arm64 .` — multi-arch cross-compile succeeds
- [ ] After merge + tag: confirm both registries show the new tag, run the published `:latest` against `/healthz`

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

Expected: prints PR URL.

### - [ ] Step 8.3: Dispatch `/review` and `/security-review` (per memory: PR review loop)

After the PR exists, run both review subagents (these are slash commands — invoke via the user's review tooling). Iterate inline comments until both reviewers are satisfied. **Do not merge** — merges require explicit user approval per memory.

---

## Self-Review Checklist (run after writing this plan)

- ✅ **Spec coverage** — every Goal in the spec maps to a task:
  - "tag a release → image appears" → Tasks 4, 5
  - "runnable as `docker run -p 8080:8080`" → Tasks 4, 6, 7
  - "CI smoke-tests the image" → Tasks 1 (route), 5 (smoke step)
  - "GitHub Release body appended with Docker pull instructions" → Task 5 (softprops step)
- ✅ **Placeholder scan** — no TBD/TODO; every code step shows real code; every command has expected output.
- ✅ **Type consistency** — `Version` (capital V, package var) is used consistently across Tasks 1, 2, and the smoke-test JSON assertion; `webui.Version` matches across daemon.go and server.go.
- ✅ **Bite-size** — every step is 2-5 minutes of real work.
