# Dockerfile + Docker Hub / GHCR publish for dicode-core

**Date**: 2026-04-27
**Closes**: [core#210](https://github.com/dicode-ayo/dicode-core/issues/210)
**Unblocks**: [core#215](https://github.com/dicode-ayo/dicode-core/issues/215) (Helm chart — separate PR)
**Reference pipeline**: [`dicode-relay/.github/workflows/release.yml`](https://github.com/dicode-ayo/dicode-relay/blob/main/.github/workflows/release.yml)

## Why

The dicode-site landing page (`site/src/components/dc-download.ts`) advertises a one-liner:

```
docker run -p 8080:8080 ghcr.io/dicode-ayo/dicode:latest
```

…but no Dockerfile lives in the dicode-core repo, no image is pushed by CI, and the registry path doesn't exist. Every "Docker" promise on the landing page is currently a 404. This spec closes that gap by adding a multi-stage Dockerfile and wiring tag-driven dual publish (Docker Hub + GHCR) into the existing release-please-driven release flow.

The relay repo already operates a working version of this exact pattern (npm + Docker on tag); this spec is mostly "port relay's `release.yml` Docker job to core, swap Node→Go".

## Goals

- Tag a release (`v*.*.*`) on `main` and have a multi-arch (amd64 + arm64) image appear on both `docker.io/dicodeayo/dicode-core` and `ghcr.io/dicode-ayo/dicode-core` within the same workflow run.
- The published image is runnable as `docker run -p 8080:8080 dicodeayo/dicode-core:<version>` and serves the dashboard.
- CI smoke-tests the freshly pushed image before marking the workflow green.
- The GitHub Release body (which release-please + goreleaser populate first) is appended with Docker pull instructions so the install path on the release page is complete.

## Non-goals

- **Helm chart** — separate PR, blocks on this one (core#215).
- **`:edge` / `:sha-*` tags from main** — explicitly deferred. Tag-only publish, driven by release-please. Can be added later if there's demand.
- **Goreleaser `dockers:` block** — chose a separate Dockerfile + `docker/build-push-action` workflow instead, matching relay. Goreleaser already handles binaries; coupling the image build to it would complicate multi-arch and diverge from the relay precedent.
- **Cosign signing** — follow-up; the chart-side PR will introduce signing for both image + chart together.
- **Landing-page string fix on dicode-site** (`dc-download.ts` says `dicode:latest`, will need `dicode-core:latest`) — separate one-line PR on the dicode-site repo, filed as an issue, not blocking this PR.
- **`/healthz` route refactor** — this spec adds a minimal route as a side-quest because the smoke test and future Helm probes need a target. Rich health introspection (DB connectivity, source reconciliation lag, etc.) is out of scope.

## Architecture

### 1. Dockerfile (repo root)

Multi-stage, cross-compiled, distroless runtime:

```dockerfile
# syntax=docker/dockerfile:1.7
ARG GO_VERSION=1.25
ARG ALPINE_VERSION=3.20

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

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/dicode /usr/local/bin/dicode
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/dicode"]
CMD ["daemon"]
```

**Design notes**

- `--platform=$BUILDPLATFORM` on the builder + `GOOS=$TARGETOS GOARCH=$TARGETARCH` on the build step → buildx cross-compiles natively on the host arch (no QEMU emulation of the Go compiler), only the resulting binaries are arch-specific. This is the standard Go-on-buildx pattern and keeps multi-arch CI under ~2 minutes.
- `gcr.io/distroless/static-debian12:nonroot` — no shell, no package manager, ~2 MB base, runs as UID 65532. Final image weighs ~35 MB (mostly the `dicode` binary).
- No `HEALTHCHECK` directive — distroless lacks `curl`/`wget`, and orchestrator-level probes (K8s livenessProbe, ECS healthCheck) are the right surface for this. Relay's Dockerfile also omits it.
- `ENTRYPOINT ["/usr/local/bin/dicode"]` + `CMD ["daemon"]` lets users override the subcommand: `docker run dicodeayo/dicode-core run my-task` works.
- `-trimpath` for reproducibility; matches what goreleaser does for the binary releases.
- `ARG VERSION` is passed in by CI from the release-please tag so `dicode version` reports the real version, not `dev`.

### 2. `.dockerignore`

Excludes everything not needed in the build context to keep the upload to the daemon snappy:

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
*.test
*.out
dicode                 # any locally-built binary
playwright.config.ts
tests/e2e
```

### 3. `/healthz` route

Add to `pkg/webui/server.go`, registered on the chi router **before** the auth middleware so unauthenticated probes work:

```go
// Register on the public router, ahead of the auth middleware group.
r.Get("/healthz", func(w http.ResponseWriter, req *http.Request) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(http.StatusOK)
    _ = json.NewEncoder(w).Encode(map[string]string{
        "status":  "ok",
        "version": Version, // package-level var, set from main.version via webui.SetVersion
    })
})
```

Plus a small unit test (`pkg/webui/healthz_test.go`) that hits the route via `httptest.NewServer` and asserts:
- 200 status
- `application/json` content type
- `status: "ok"` in the body
- The route bypasses auth (test with `cfg.Server.Auth = true`).

The `version` field reuses whatever the existing `version` symbol is in `cmd/dicode/main.go`. If exposing it cleanly to `pkg/webui` requires plumbing, the unit-test fallback is for `version` to be `"dev"` — fine for this PR.

### 4. CI workflow — extend `.github/workflows/release-please.yml`

Add a third job, `publish-docker`, gated on `needs.release-please.outputs.release_created`. Runs in parallel with the existing `goreleaser` job (no ordering needed — they touch disjoint outputs).

```yaml
publish-docker:
  name: Publish Docker image
  needs: release-please
  if: ${{ needs.release-please.outputs.release_created }}
  runs-on: ubuntu-latest
  permissions:
    contents: write   # for action-gh-release append
    packages: write   # for ghcr.io
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

    - name: Smoke test pushed image
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
```

**Why these specific choices**

- `docker/metadata-action@v6` with `value=${{ needs.release-please.outputs.tag_name }}` — release-please's tag is the source of truth for the version; we don't rely on `git describe` or push events.
- `cache-from: type=gha` — GitHub Actions cache reuses Go module + build cache layers across runs, dropping cold-build time from ~3 min to ~1 min.
- The smoke test uses `ghcr.io` (not Docker Hub) because GHCR pulls don't count against Docker Hub anonymous rate limits and propagation is faster.
- `softprops/action-gh-release@v3` with `append_body: true` — release-please + goreleaser populate the release body first (changelog + binary install). This step appends Docker instructions without overwriting.

### 5. Documentation

**`README.md`** — add a "Docker" section under "Install":

```markdown
## Docker

```sh
docker pull dicodeayo/dicode-core:latest
docker run -d --name dicode \
  -p 8080:8080 \
  -v dicode-data:/data \
  dicodeayo/dicode-core:latest
```

Open http://localhost:8080. SQLite state persists in the named volume `dicode-data`.

Multi-arch images are published for `linux/amd64` and `linux/arm64`. The image
is also mirrored on GHCR: `ghcr.io/dicode-ayo/dicode-core`.
```

**`docs-src/getting-started/index.md`** — add a "Run with Docker" subsection mirroring the README content, plus a `docker-compose.yml` example for users who want auto-restart and named env-file mounting.

## Data flow

```
release-please PR merge to main
        │
        ├─► release-please job: creates v0.1.1 tag + GitHub Release (body = changelog)
        │
        ├─► goreleaser job (existing):                publish-docker job (NEW):
        │     - go build linux/amd64/arm64 +            - buildx → multi-arch image
        │       darwin + windows                          - push docker.io + ghcr.io
        │     - upload archives + checksums to            - smoke test :version
        │       GitHub Release                            - append Docker install
        │                                                   block to GitHub Release
        ▼                                                ▼
                       Single GitHub Release with: changelog, binaries, image links
```

The two release-time jobs run in parallel and append to the same Release; `softprops/action-gh-release@v3` is safe to run twice on the same release because `append_body: true` and goreleaser doesn't fight it (goreleaser writes the binary list directly via the GitHub Releases API, not through `softprops`).

## Failure modes

| Failure | Behavior | Recovery |
|---|---|---|
| Docker Hub login fails (creds wrong/missing) | Workflow fails at login step before any push | Set `DOCKERHUB_USERNAME` / `DOCKERHUB_TOKEN` org secrets; re-run the workflow on the existing tag (`gh workflow run release-please.yml`) |
| Multi-arch build fails for arm64 only | `build-push-action` exits non-zero, no partial push (buildx is atomic per `--push`) | Fix Dockerfile, push a patch tag |
| Smoke test fails (image runs but `/healthz` doesn't respond) | Workflow fails after push | Image is already published; either `gh release delete-asset` to scrub Docker mention from release body, or push a patch tag with a fix. Document that this means the GH Release will be missing the Docker block. |
| GHCR push succeeds, Docker Hub push fails | `build-push-action` is single-step dual-push: either both succeed or the action fails | Re-run; idempotent |
| `release-please` runs but `release_created=false` (no eligible commits) | All downstream jobs skipped via `if:` gate | Normal — nothing to publish |

## Testing

1. **Local Dockerfile build** (CI-equivalent): `docker buildx build --platform linux/amd64,linux/arm64 -t dicode-core:test .` — must succeed.
2. **Local smoke test**: `docker run -p 8080:8080 dicode-core:test` followed by `curl http://localhost:8080/healthz` returning 200.
3. **`/healthz` unit test**: `go test ./pkg/webui/...` covers the new route, including the auth-bypass case.
4. **CI dry run on a feature branch**: cannot fully exercise the publish path without a tag. Workaround: a one-off branch workflow gated on `workflow_dispatch` that runs the same buildx + smoke steps but pushes to a throwaway `:pr-<n>` tag on a personal Docker Hub account. **Not** included in this PR — verified manually by the implementer instead.
5. **Post-merge verification on the next real release**: after `release-please-pr` merges and the tag is cut, watch the workflow, confirm both registries show the new tag, run the documented `docker run` one-liner, hit the dashboard.

## Files touched

```
NEW   Dockerfile
NEW   .dockerignore
EDIT  .github/workflows/release-please.yml          (+publish-docker job, ~75 lines)
EDIT  pkg/webui/server.go                           (+/healthz handler, ~15 lines)
NEW   pkg/webui/healthz_test.go                     (~50 lines)
EDIT  README.md                                     (+Docker section, ~25 lines)
EDIT  docs-src/getting-started/index.md             (+Docker subsection, ~30 lines)
```

Total expected diff: ~250 lines added, 0 removed.

## Open questions / risks

- **Image version reporting**: the `dicode` binary stamps `main.version` from `-ldflags`. The `/healthz` JSON should expose this so smoke tests can assert "the image actually has the version it claims." Plumbing `main.version` into `pkg/webui` requires a small refactor (currently `cmd/dicode/main.go` keeps it package-private). Acceptable for this PR: webui exposes a package-level `Version` var that `cmd/dicode/main.go` sets at startup, or the healthz handler reads it from a config-time argument. Decision: package-level `Version` var in `pkg/webui`, set from `main.go` before `webui.New()` is called. Noted in implementation plan.
- **Multi-arch CI cost**: estimated +2 min per release (cold cache). Acceptable — releases are infrequent and cache hits drop this to ~1 min.
- **Distroless tag pinning**: `gcr.io/distroless/static-debian12:nonroot` is a moving tag. Acceptable for now (Google's distroless images are well-curated); if reproducibility becomes a concern, pin to a digest in a follow-up.
