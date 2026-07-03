// Package python executes Python task scripts via the managed uv binary.
//
// uv (https://github.com/astral-sh/uv) is a fast Python package manager and
// script runner that dicode downloads and caches automatically — no system
// Python or pip installation is required.
//
// # Execution model
//
// Each Run spawns a fresh uv subprocess connected to the same per-run Unix
// socket server used by the Deno runtime. An embedded Python shim (sdk.py)
// provides the same globals as the Deno SDK:
//
//	log, params, env, kv, input, output
//
// To return a value from a task, assign the module-level variable `result`:
//
//	result = {"count": 42}
//
// # PEP 723 inline dependencies
//
// uv supports inline dependency declarations inside the script:
//
//	# /// script
//	# dependencies = ["requests>=2.31", "boto3"]
//	# ///
//
// The runtime extracts any such block from task.py and places it at the top of
// the temporary wrapper file so that uv can parse it correctly.
//
// When a task.py.lock sidecar exists (written by `dicode python relock` via
// `uv lock --script`), it is staged next to the wrapper and enforced with
// `uv run --locked`, pinning resolution to the recorded versions and hashes.
// Without a sidecar the task runs unlocked, exactly as before (issue #465).
//
// # Permission enforcement
//
// Declared permissions.{fs,net,run} are enforced by a PEP 578 audit hook
// injected into the wrapper (see guard.go and sdk/guard.py). The hook is a
// guardrail on declared intent, not a security boundary: fs reads and sys
// are unenforced, and in-process escapes are acceptable.
package python

import (
	"bufio"
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/runtime/envresolve"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
	uvpkg "github.com/dicode/dicode/pkg/uv"
	"go.uber.org/zap"
)

//go:embed sdk/dicode_sdk.py
var sdkContent string

// Runtime is the ManagedRuntime implementation for Python+uv.
// It manages the uv binary lifecycle and creates socket-bridge Executors.
//
// The Set* dependency wiring (SetEngine, SetGateway, SetInputStore, ...) is
// promoted from the embedded pkgruntime.BridgeDeps — see bridgedeps.go for
// the full surface shared with the Deno runtime. Executors read the
// late-wired fields (InputStore, Replayer, SourceMgr, RepoResolver,
// TestGuard, ProtectedPaths) live from the parent's BridgeDeps at IPC server
// creation time.
type Runtime struct {
	pkgruntime.BridgeDeps
}

// New creates a Python Runtime manager.
func New(reg *registry.Registry, sc secrets.Chain, database db.DB, log *zap.Logger) (*Runtime, error) {
	secret, err := ipc.NewSecret()
	if err != nil {
		return nil, fmt.Errorf("python runtime: generate ipc secret: %w", err)
	}
	return &Runtime{
		BridgeDeps: pkgruntime.BridgeDeps{Registry: reg, SecretsChain: sc, DB: database, Log: log, IPCSecret: secret},
	}, nil
}

// --- ManagedRuntime interface ---

func (rt *Runtime) Name() string        { return "python" }
func (rt *Runtime) DisplayName() string { return "Python (uv)" }
func (rt *Runtime) Description() string {
	return "Python runtime managed by uv. Supports inline dependencies via PEP 723 (# /// script blocks). Full SDK globals: log, params, env, kv, input, output."
}
func (rt *Runtime) DefaultVersion() string { return uvpkg.DefaultVersion }

// BinaryPath returns the expected cache path for the uv binary at the given version.
func (rt *Runtime) BinaryPath(version string) (string, error) {
	return uvpkg.BinaryPath(version)
}

// IsInstalled reports whether the uv binary for the given version is cached.
func (rt *Runtime) IsInstalled(version string) bool {
	p, err := uvpkg.BinaryPath(version)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// Install downloads and caches the uv binary for the given version.
func (rt *Runtime) Install(_ context.Context, version string) error {
	_, err := uvpkg.EnsureUv(version)
	return err
}

// NewExecutor returns an Executor that runs Python scripts via the uv binary
// at binaryPath, connected to the dicode socket-bridge SDK.
//
// Note: this copy list is intentionally wider than the Deno runtime's
// NewExecutor — it also snapshots SecretsManager, IPCSecret, Engine, and
// Gateway. Preserved as-is by the #388 dedup (behavior-preserving); see that
// issue for the follow-up on reconciling the two.
func (rt *Runtime) NewExecutor(binaryPath string) pkgruntime.Executor {
	return &executor{
		uvPath: binaryPath,
		parent: rt,
		BridgeDeps: pkgruntime.BridgeDeps{
			Registry:       rt.Registry,
			SecretsChain:   rt.SecretsChain,
			SecretsManager: rt.SecretsManager,
			DB:             rt.DB,
			Log:            rt.Log,
			IPCSecret:      rt.IPCSecret,
			Engine:         rt.Engine,
			Gateway:        rt.Gateway,
			SecretOutputCh: rt.SecretOutputCh,
			ProviderRunner: rt.ProviderRunner,
		},
	}
}

// --- executor ---

type executor struct {
	uvPath string
	// parent is the back-reference for live lookups: the late-wired fields
	// (InputStore, Replayer, SourceMgr, RepoResolver, TestGuard,
	// ProtectedPaths, SharedResolver) are not snapshotted below — they are
	// read from parent.BridgeDeps at IPC server creation time so daemon
	// wiring that happens after NewExecutor is visible to all executors
	// without any extra bookkeeping.
	parent *Runtime
	// BridgeDeps is the construction-time snapshot of the manager's deps
	// taken by NewExecutor (see its doc for the copy list).
	pkgruntime.BridgeDeps
}

// envresolver returns the env resolver to use for an Execute call, reading
// the daemon-scoped shared resolver (issue #242) live through parent. See
// pkgruntime.LiveResolver for the precedence order.
func (e *executor) envresolver() *envresolve.Resolver {
	var parent *pkgruntime.BridgeDeps
	if e.parent != nil {
		parent = &e.parent.BridgeDeps
	}
	return pkgruntime.LiveResolver(&e.BridgeDeps, parent)
}

// Execute implements runtime.Executor.
func (e *executor) Execute(ctx context.Context, spec *task.Spec, opts pkgruntime.RunOptions) (*pkgruntime.RunResult, error) {
	runID := opts.RunID
	result := &pkgruntime.RunResult{RunID: runID}

	// A failed run must always leave a diagnostic in the run log. Early
	// returns (env resolution, missing script, socket/wrapper setup, uv
	// start) set result.Error before any subprocess stderr exists; and a
	// subprocess that exits non-zero without emitting stderr (signal kill,
	// OOM) leaves the streamed-stderr log empty. Appending result.Error here
	// guarantees the WebUI run detail and CLI log output show why it failed.
	defer func() {
		if result.Error != nil {
			_ = e.Registry.AppendLog(context.Background(), runID, "error", result.Error.Error())
		}
	}()

	// Resolve declared env permissions — preferring the trigger engine's
	// preflight result over inline resolution (see ResolveRunEnv).
	resolved, redactor, err := pkgruntime.ResolveRunEnv(ctx, spec, opts.PreResolvedEnv, e.envresolver)
	if err != nil {
		result.Error = err
		return result, nil
	}

	// Read the user's task.py.
	scriptPath := spec.ScriptPath()
	if scriptPath == "" {
		result.Error = fmt.Errorf("script not found for task %s", spec.ID)
		return result, nil
	}
	scriptBytes, err := os.ReadFile(scriptPath) //nolint:gosec
	if err != nil {
		result.Error = fmt.Errorf("read script: %w", err)
		return result, nil
	}

	execCtx, cancel := pkgruntime.ExecContext(ctx, spec.Timeout)
	defer cancel()

	mergedParams := pkgruntime.MergeParams(spec.Params, opts.Params)

	srv := e.BridgeDeps.NewIPCServer(runID, spec, mergedParams, opts.Input, redactor, &e.parent.BridgeDeps)
	socketPath, token, err := srv.Start(execCtx)
	if err != nil {
		result.Error = fmt.Errorf("start socket server: %w", err)
		return result, nil
	}
	defer srv.Stop()

	// Build the temporary wrapper file.
	wrapped, err := buildWrapper(scriptBytes, buildGuardPolicy(spec, socketPath, e.parent.ProtectedPaths))
	if err != nil {
		result.Error = fmt.Errorf("build wrapper: %w", err)
		return result, nil
	}

	tmpFile, err := os.CreateTemp("", "dicode-task-"+runID+"__*.py")
	if err != nil {
		result.Error = fmt.Errorf("create temp file: %w", err)
		return result, nil
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(wrapped); err != nil {
		tmpFile.Close()
		result.Error = fmt.Errorf("write wrapper: %w", err)
		return result, nil
	}
	tmpFile.Close()

	// Reproducible dependency resolution (issue #465): when the task has a
	// committed lock sidecar (task.py.lock, from `dicode python relock`),
	// stage it next to the wrapper and run with --locked so drift fails
	// loudly instead of silently resolving new versions. Tasks without a
	// sidecar run exactly as before — mirrors the Deno runtime, which only
	// enforces --frozen when a deno.lock is present.
	stagedLock, locked, err := stageLockSidecar(scriptPath, tmpFile.Name())
	if err != nil {
		result.Error = err
		return result, nil
	}
	if locked {
		defer os.Remove(stagedLock)
	}

	cmd := exec.CommandContext(execCtx, e.uvPath, buildUvRunArgs(tmpFile.Name(), locked)...) //nolint:gosec
	cmd.Env = pkgruntime.SubprocessEnv(spec, resolved, socketPath, token)

	stderr, err := cmd.StderrPipe()
	if err != nil {
		result.Error = err
		return result, nil
	}

	if err := cmd.Start(); err != nil {
		result.Error = fmt.Errorf("start uv: %w", err)
		return result, nil
	}

	// Stream uv/Python stderr to registry logs in real-time.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			_ = e.Registry.AppendLog(context.Background(), runID, "warn", redactor.RedactString(scanner.Text()))
		}
		if err := scanner.Err(); err != nil {
			e.Log.Warn("stderr scanner error", zap.String("run", runID), zap.Error(err))
		}
	}()

	doneCh := make(chan error, 1)
	go func() { doneCh <- cmd.Wait() }()

	exitErr, exitedFirst := pkgruntime.AwaitBridgeCompletion(srv.ReturnCh(), doneCh, pkgruntime.BridgeShutdownGrace,
		func(retVal any) {
			result.ChainInput = retVal
			if out := srv.Output(); out != nil {
				result.ChainInput = out.Data
			}
		},
		func() { _ = cmd.Process.Signal(syscall.SIGTERM) },
	)
	if exitedFirst {
		if out := srv.Output(); out != nil {
			result.ChainInput = out.Data
		}
		if exitErr != nil {
			result.Error = exitErr
		}
	}

	wg.Wait()
	return result, nil
}

// buildWrapper assembles the final Python file that uv will execute:
//
//  1. PEP 723 script block (extracted from the user script, if present) — must
//     be first so uv can parse inline dependencies.
//  2. The dicode SDK shim (sdk.py).
//  3. The permission guard (guard.py) — after the SDK so the hook never
//     governs the SDK's own socket setup, before the task body so it governs
//     all user code. uv resolves PEP 723 deps before the script runs, so
//     dependency installation is ungoverned too.
//  4. The user script body (script block stripped out).
//  5. Return-capture epilogue.
func buildWrapper(scriptBytes []byte, pol guardPolicy) (string, error) {
	pep723, body := extractPEP723(string(scriptBytes))

	guard, err := buildGuard(pol)
	if err != nil {
		return "", err
	}

	var w strings.Builder
	if pep723 != "" {
		w.WriteString(pep723)
		w.WriteString("\n")
	}
	w.WriteString("# === dicode SDK ===\n")
	w.WriteString(sdkContent)
	w.WriteString("\n# === permission guard ===\n")
	w.WriteString(guard)
	w.WriteString("\n# === task script ===\n")
	w.WriteString(body)
	w.WriteString("\n# === return capture ===\n")
	w.WriteString("import sys as _sys\n")
	w.WriteString("_asyncio_mod = _sys.modules['asyncio']\n")
	w.WriteString("_main = globals().get('main')\n")
	w.WriteString("if _main is not None and _asyncio_mod.iscoroutinefunction(_main):\n")
	w.WriteString("    result = _asyncio_mod.run(_main())\n")
	w.WriteString("_set_return(globals().get('result', None))\n")
	// Schedule close on _loop so it runs *after* any pending _fire coroutines
	// (the event loop is FIFO — tasks submitted before this will drain first).
	// Wrap in try/except so a timeout never marks a successful run as failed.
	w.WriteString("async def _dicode_close():\n")
	w.WriteString("    _writer.close()\n")
	w.WriteString("    await _writer.wait_closed()\n")
	w.WriteString("try:\n")
	w.WriteString("    _asyncio_mod.run_coroutine_threadsafe(_dicode_close(), _loop).result(timeout=5)\n")
	w.WriteString("except Exception:\n")
	w.WriteString("    pass\n")
	return w.String(), nil
}

// extractPEP723 splits a Python script into the PEP 723 script block (if any)
// and the remaining body. The script block is the first contiguous group of
// lines starting with "# /// script" and ending with "# ///".
func extractPEP723(src string) (block, body string) {
	lines := strings.Split(src, "\n")
	start := -1
	end := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if start == -1 && trimmed == "# /// script" {
			start = i
			continue
		}
		if start != -1 && end == -1 && trimmed == "# ///" {
			end = i
			break
		}
	}
	if start == -1 || end == -1 {
		return "", src
	}
	blockLines := lines[start : end+1]
	bodyLines := append(lines[:start:start], lines[end+1:]...)
	return strings.Join(blockLines, "\n"), strings.Join(bodyLines, "\n")
}
