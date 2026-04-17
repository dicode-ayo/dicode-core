# Single Binary: Merge dicoded into dicode

## Problem

dicode ships as two binaries: `dicode` (CLI) and `dicoded` (daemon). The CLI auto-starts the daemon by searching for `dicoded` next to itself or on `$PATH`. This fails if the user only downloads `dicode`, producing a confusing error: "dicoded binary not found; start the daemon manually."

Users should download one binary and have everything work.

## Solution

Merge the daemon into the `dicode` binary behind a `dicode daemon` subcommand. The CLI auto-start re-execs itself (`dicode daemon`) instead of searching for a separate binary.

## Changes

### New: `pkg/daemon/daemon.go`

Exported entry point for the daemon process:

```go
func Run(ctx context.Context, cancel context.CancelFunc, configPath string) error
```

Contains all logic currently in `cmd/dicoded/main.go`:
- Config loading + onboarding check
- Logger setup (returns `*zap.Logger` + `*webui.LogBroadcaster`)
- The full `run()` function (db, secrets, registry, runtimes, reconciler, webui, control socket, relay, tray)
- Helper functions: `buildRuntimes`, `buildSources`, `buildSecretsChain`, `buildLogger`, `deriveBrokerBaseURL`

The `version` variable is set via an exported `var Version string` that `cmd/dicode/main.go` populates from its own `ldflags`-injected version.

### Modified: `cmd/dicode/main.go`

1. Add `case "daemon"` to `dispatch()` — parses `--config` flag from remaining args, calls `daemon.Run()`
2. `ensureDaemon()` — replace the `dicoded` binary search with:
   ```go
   self, _ := os.Executable()
   cmd := exec.Command(self, "daemon")
   ```
3. Update `usage()` help text to include `daemon` subcommand
4. Set `daemon.Version = version` in `main()`

### Deleted: `cmd/dicoded/`

Entire directory removed. No backward compatibility shim.

### Modified: `Makefile`

```makefile
BINARY := dicode
CMD    := ./cmd/dicode

build:
    $(GO) build $(GOFLAGS) -o $(BINARY) $(CMD)

run: build
    ./$(BINARY) daemon
```

Remove `DAEMON_BINARY`, `DAEMON_CMD`, and all `dicoded` references.

### Modified: `.goreleaser.yaml`

Remove the `dicoded` build entry. Single build:

```yaml
builds:
  - id: dicode
    main: ./cmd/dicode/
    binary: dicode
    ...

archives:
  - id: dicode
    builds: [dicode]
```

### Modified: `.gitignore`

Remove `dicoded` line. Keep `dicode`.

### Modified: `CLAUDE.md`

Update commands:
- `make build` → "compile ./dicode binary"
- `make run` → "build and run daemon (Ctrl-C to stop)"

### Modified: `README.md`

- Line 27: "dicode runs as two binaries..." → "dicode is a single binary..."
- Architecture diagram: remove `dicoded` label, just show `dicode daemon`
- Deployment section: `dicoded` → `dicode daemon`
- Project structure: remove `cmd/dicoded/` entry

### Modified: `docs/current-state.md`

- Replace `cmd/dicoded/main.go` section with `pkg/daemon/` section
- Update `cmd/dicode/main.go` section to mention `daemon` subcommand
- Update build commands

### Modified: `docs/implementation-plan.md`

- Update M7 wiring description to reference `pkg/daemon/`

## What stays the same

- Control socket protocol — unchanged
- CLI subcommands (`run`, `list`, `logs`, `status`, `secrets`, `relay`) — unchanged
- Auto-start behavior — same polling, same 8s timeout, same daemon.log capture
- Config file format — unchanged
- All runtime, trigger, webui, relay code — unchanged (just moved up one package)

## Testing

- `go build ./cmd/dicode` produces a single binary
- `./dicode daemon` starts the full engine (same as old `dicoded`)
- `./dicode list` auto-starts `dicode daemon` in background if not running
- `go test ./pkg/daemon/...` — no new tests needed (the package is pure wiring)
- All existing tests pass unchanged
