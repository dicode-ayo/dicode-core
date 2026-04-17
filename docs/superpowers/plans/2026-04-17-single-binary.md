# Single Binary Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Merge the `dicoded` daemon into the `dicode` CLI binary so users download one binary and everything works.

**Architecture:** Move all daemon logic from `cmd/dicoded/main.go` into a new `pkg/daemon/` package with an exported `Run()` function. Add a `dicode daemon` subcommand that calls it. Change `ensureDaemon()` to re-exec `dicode daemon` instead of searching for a separate `dicoded` binary. Delete `cmd/dicoded/`.

**Tech Stack:** Go, existing project packages (no new dependencies)

---

### Task 1: Create `pkg/daemon/daemon.go`

**Files:**
- Create: `pkg/daemon/daemon.go`

- [ ] **Step 1: Create the package with all daemon logic**

Move the entire contents of `cmd/dicoded/main.go` into `pkg/daemon/daemon.go`. Change the package from `main` to `daemon`. Replace the `main()` function with an exported `Run()` function. Replace the package-level `var version` with an exported `var Version string`.

The file should:
- Package: `daemon`
- Export `var Version string` (set by the CLI binary before calling `Run`)
- Export `func Run(configPath string)` — this replaces `main()`. It handles signal setup, config loading, onboarding, logger creation, and calls the private `run()` function. On fatal error it prints to stderr and calls `os.Exit(1)`.
- Keep all private functions unchanged: `run()`, `buildRuntimes()`, `buildSources()`, `buildSecretsChain()`, `buildLogger()`, `deriveBrokerBaseURL()`, `sourceNameFor()`, `buildTaskSetSource()`
- Replace all references to `version` with `Version`

- [ ] **Step 2: Verify it compiles**

Run: `cd /workspaces/dicode-core && go build ./pkg/daemon/`
Expected: compiles with no errors

- [ ] **Step 3: Commit**

```bash
git add pkg/daemon/daemon.go
git commit -m "refactor: extract daemon logic into pkg/daemon"
```

---

### Task 2: Wire `dicode daemon` subcommand and update `ensureDaemon`

**Files:**
- Modify: `cmd/dicode/main.go`

- [ ] **Step 1: Add `daemon` to dispatch and update `main()`**

In `main()`, before the `ensureDaemon` call, check if `os.Args[1] == "daemon"`. If so, parse `--config` from remaining args (default `"dicode.yaml"`), set `daemon.Version = version`, call `daemon.Run(configPath)`, and return. This must happen before `ensureDaemon` — the daemon subcommand doesn't connect to an existing daemon, it IS the daemon.

Add the `daemon` import: `"github.com/dicode/dicode/pkg/daemon"`

Also add `daemon` to the `usage()` help text.

- [ ] **Step 2: Simplify `ensureDaemon` to re-exec self**

Replace the `dicoded` binary search logic in `ensureDaemon()` with:

```go
self, err := os.Executable()
if err != nil {
    return err
}
cmd := exec.Command(self, "daemon")
```

Remove the `dicoded` search, the `filepath.Join(filepath.Dir(self), "dicoded")` block, and the `exec.LookPath("dicoded")` fallback. Update the error message from "dicoded binary not found" to something like "failed to start daemon". Update the comment from "starts dicoded" to "starts the daemon".

- [ ] **Step 3: Verify it compiles**

Run: `cd /workspaces/dicode-core && go build ./cmd/dicode/`
Expected: compiles with no errors

- [ ] **Step 4: Commit**

```bash
git add cmd/dicode/main.go
git commit -m "feat: add 'dicode daemon' subcommand, re-exec self in ensureDaemon"
```

---

### Task 3: Delete `cmd/dicoded/`

**Files:**
- Delete: `cmd/dicoded/main.go`

- [ ] **Step 1: Remove the directory**

```bash
rm -rf cmd/dicoded/
```

- [ ] **Step 2: Verify full build**

Run: `cd /workspaces/dicode-core && go build ./...`
Expected: compiles with no errors (nothing should import `cmd/dicoded`)

- [ ] **Step 3: Run all tests**

Run: `cd /workspaces/dicode-core && go test ./... -timeout 60s`
Expected: all tests pass

- [ ] **Step 4: Commit**

```bash
git add -A
git commit -m "refactor: remove cmd/dicoded — daemon is now 'dicode daemon'"
```

---

### Task 4: Update Makefile

**Files:**
- Modify: `Makefile`

- [ ] **Step 1: Simplify to single binary**

Replace the full Makefile with:

```makefile
BINARY  := dicode
CMD     := ./cmd/dicode
VERSION ?= dev

GO      := $(shell which go 2>/dev/null || echo $(HOME)/.local/share/mise/shims/go)
GOFLAGS := -ldflags "-X main.version=$(VERSION)"

.PHONY: build test test-verbose test-race lint fmt clean run tidy help

## build: compile the dicode binary
build:
	$(GO) build $(GOFLAGS) -o $(BINARY) $(CMD)

## run: build and run the daemon (Ctrl-C to stop)
run: build
	./$(BINARY) daemon

## test: run all tests
test:
	$(GO) test ./... -timeout 60s

## test-verbose: run all tests with verbose output
test-verbose:
	$(GO) test ./... -timeout 60s -v

## test-race: run tests with the race detector
test-race:
	$(GO) test ./... -timeout 60s -race

## tidy: tidy go.mod and go.sum
tidy:
	$(GO) mod tidy

## fmt: format all Go source files
fmt:
	$(GO) fmt ./...

## lint: format and vet all Go source files
lint: fmt
	$(GO) vet ./...

## clean: remove compiled binary
clean:
	rm -f $(BINARY)

## help: list available targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'
```

- [ ] **Step 2: Verify make build works**

Run: `cd /workspaces/dicode-core && make clean && make build`
Expected: produces single `dicode` binary

- [ ] **Step 3: Commit**

```bash
git add Makefile
git commit -m "build: simplify Makefile to single binary"
```

---

### Task 5: Update GoReleaser and .gitignore

**Files:**
- Modify: `.goreleaser.yaml`
- Modify: `.gitignore`

- [ ] **Step 1: Remove dicoded from GoReleaser**

Replace the `builds:` section to have only one build (remove the `dicoded` entry). Update the `archives:` section to reference only `[dicode]`:

```yaml
builds:
  - id: dicode
    main: ./cmd/dicode/
    binary: dicode
    env:
      - CGO_ENABLED=0
    goos:
      - linux
      - darwin
      - windows
    goarch:
      - amd64
      - arm64
    ignore:
      - goos: windows
        goarch: arm64
    ldflags:
      - -s -w -X main.version={{.Version}}

archives:
  - id: dicode
    builds: [dicode]
```

- [ ] **Step 2: Remove dicoded from .gitignore**

Remove the `dicoded` line from `.gitignore`. Keep the `dicode` line.

- [ ] **Step 3: Commit**

```bash
git add .goreleaser.yaml .gitignore
git commit -m "build: remove dicoded from goreleaser and gitignore"
```

---

### Task 6: Update CLAUDE.md

**Files:**
- Modify: `CLAUDE.md`

- [ ] **Step 1: Update commands section**

Change `make build` description to "compile ./dicode binary" (remove "both" and "dicoded").
Change `make run` description to "build and run daemon (Ctrl-C to stop)".

In the Architecture section, update:
- "dicode runs as two binaries: `dicoded` (daemon) and `dicode` (CLI)" → remove this or change to "dicode is a single binary with CLI and daemon modes"

In the Startup sequence, change `cmd/dicode/main.go` reference if it mentions `cmd/dicoded`.

- [ ] **Step 2: Commit**

```bash
git add CLAUDE.md
git commit -m "docs: update CLAUDE.md for single binary"
```

---

### Task 7: Update README.md

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Update two-binary references**

Key changes:
- Line ~27: "dicode runs as two binaries: `dicoded` (daemon — ...) and `dicode` (CLI — ...)" → "dicode is a single binary. Run `dicode daemon` to start the engine, or use any CLI subcommand (which auto-starts the daemon in the background)."
- Architecture diagram: change `dicoded (daemon)` label to `dicode daemon`
- Deployment section: replace `dicoded` with `dicode daemon`
- Service management section: replace `dicoded` with `dicode daemon`, `sudo systemctl start dicoded` → `sudo systemctl start dicode`
- Docker section: update if it references `dicoded` entrypoint
- Project structure tree: remove `cmd/dicoded/` line, add `pkg/daemon/` line

- [ ] **Step 2: Commit**

```bash
git add README.md
git commit -m "docs: update README for single binary architecture"
```

---

### Task 8: Update docs/current-state.md and docs/implementation-plan.md

**Files:**
- Modify: `docs/current-state.md`
- Modify: `docs/implementation-plan.md`

- [ ] **Step 1: Update current-state.md**

- Replace the `### cmd/dicoded/main.go ✅ (daemon binary)` section with a `### pkg/daemon/ ✅` section describing the exported `Run()` entry point
- Update the `### cmd/dicode/main.go ✅ (CLI binary)` section to mention the `daemon` subcommand
- Update the build commands section: `make build` now produces one binary
- Update auto-start description: "locates `dicoded`" → "re-execs `dicode daemon`"

- [ ] **Step 2: Update implementation-plan.md**

- Update M7 description to reference `pkg/daemon/` instead of `cmd/dicoded/main.go`

- [ ] **Step 3: Commit**

```bash
git add docs/current-state.md docs/implementation-plan.md
git commit -m "docs: update current-state and implementation-plan for single binary"
```

---

### Task 9: Final verification

- [ ] **Step 1: Clean build**

```bash
cd /workspaces/dicode-core && make clean && make build
```

Expected: single `dicode` binary produced, no `dicoded`

- [ ] **Step 2: Run all tests**

```bash
go test ./... -timeout 60s
```

Expected: all tests pass

- [ ] **Step 3: Verify `dicode daemon --help` works**

```bash
./dicode daemon --help
```

Expected: shows config flag usage or starts daemon

- [ ] **Step 4: Verify no remaining dicoded references in code**

```bash
grep -r 'dicoded' --include='*.go' --include='*.yaml' --include='*.yml' --include='*.md' --include='Makefile' . | grep -v '.git/' | grep -v 'docs/superpowers/' | grep -v vendor/
```

Expected: no results (or only historical references in changelog)
