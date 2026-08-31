// Package python executes Python task scripts via the managed uv binary.
//
// uv (https://github.com/astral-sh/uv) is a fast Python package manager and
// script runner that dicode downloads and caches automatically — no system
// Python or pip installation is required.
//
// # Execution model
//
// Each Run spawns a fresh uv subprocess connected to the same per-run Unix
// socket server used by the Deno runtime. An embedded Python shim (dicode_sdk.py)
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
	"context"
	_ "embed"
	"fmt"
	"io"
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
// This copy list matches the Deno runtime's NewExecutor field-for-field
// (issue #718) — see that function's doc for why copying SecretsManager,
// IPCSecret, Engine, and Gateway here (unlike the pre-#718 Deno list) is
// both necessary and safe.
//
// The provider secret-output channel is deliberately NOT in this snapshot
// (issue #719) — see the Deno NewExecutor's doc for why: it is per-run
// state that flows through runtime.RunOptions.SecretOutputCh instead, so
// this executor observes whatever channel the engine wired for the run
// currently in flight rather than a value frozen (always nil) at
// construction time.
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

	// Ephemeral per-run MCP token (opt-in via permissions.env
	// DICODE_MCP_API_KEY): mint before anything else touches resolved, and
	// defer the revoke unconditionally so it fires on every exit path below.
	mcpToken, mcpRevoke, err := pkgruntime.ApplyMCPToken(ctx, &e.parent.BridgeDeps, e.Log, spec, runID, resolved)
	if err != nil {
		result.Error = err
		return result, nil
	}
	defer mcpRevoke()
	// The redactor was snapshot before the mint; fold the token in so it is
	// scrubbed from run logs like any other secret.
	if mcpToken != "" {
		redactor = redactor.WithExtra(mcpToken)
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

	// The IPC server spans the whole run — both the initial attempt and any
	// stale-lock retry — so it is scoped to a run-lifetime context, not a
	// per-attempt timeout. Each subprocess attempt gets its own fresh
	// ExecContext (see runOnce) so the retry, which follows a possibly slow
	// relock, is born with a full timeout budget rather than the initial
	// attempt's already-spent one.
	srvCtx, srvCancel := context.WithCancel(ctx)
	defer srvCancel()

	mergedParams := pkgruntime.MergeParams(spec.Params, opts.Params)

	srv := e.BridgeDeps.NewIPCServer(runID, spec, mergedParams, opts.Input, redactor, &e.parent.BridgeDeps)
	// Per-run provider secret-output channel (issue #719): set only on the
	// one run the trigger engine is actually firing as a provider.
	if opts.SecretOutputCh != nil {
		srv.SetSecretOutput(opts.SecretOutputCh)
	}
	// Inject a resumed run's prior state + user input (#95); nil on first run.
	srv.SetResume(opts.Resumed, opts.ResumeState, opts.ResumeInput)
	socketPath, token, err := srv.Start(srvCtx)
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
	//
	// Staging happens per attempt so a stale-lock auto-recovery relock (issue
	// #455) is picked up on retry: uv discovers the lock strictly by the
	// <wrapper>.lock filename, so the regenerated sidecar must be re-copied.
	stagedPath := LockSidecarPath(tmpFile.Name())
	defer os.Remove(stagedPath)

	// runOnce stages the lock, spawns the uv subprocess, streams its output, and
	// records the outcome on result. It reports whether the run failed with the
	// stale-lock signature so a single deterministic relock+retry can recover a
	// drifted sidecar without invoking the AI auto-fix loop.
	runOnce := func() (staleLock bool) {
		result.Error = nil
		result.ChainInput = nil
		result.ReturnValue = nil
		result.OutputContentType = ""
		result.OutputContent = ""

		_, locked, err := stageLockSidecar(scriptPath, tmpFile.Name())
		if err != nil {
			result.Error = err
			return false
		}

		// Fresh per-attempt timeout budget: the retry's deadline is measured
		// from here (after any relock), so a slow relock cannot starve it.
		execCtx, cancel := pkgruntime.ExecContext(ctx, spec.Timeout)
		defer cancel()

		cmd := exec.CommandContext(execCtx, e.uvPath, buildUvRunArgs(tmpFile.Name(), locked)...) //nolint:gosec
		cmd.Env = pkgruntime.SubprocessEnv(spec, resolved, socketPath, token)
		sniffer := pkgruntime.NewLockErrSniffer(staleLockSignature)

		// Route stderr through cmd.Stderr (a writer), not StderrPipe: this makes
		// cmd.Wait block until every stderr byte has been copied to the sniffer,
		// so the stale-lock decision never races the process exit closing a
		// pipe. The bytes are teed to a pipe that feeds the log streamer, so they
		// still reach the run log. Stream at "warn": the stream mixes uv's own
		// progress chatter with Python tracebacks, unlike Deno's, whose stderr is
		// logged as "error". Stdout is not streamed (Python tasks log via the
		// SDK's log global over IPC).
		pr, pw := io.Pipe()
		cmd.Stderr = io.MultiWriter(sniffer, pw)
		if err := cmd.Start(); err != nil {
			result.Error = fmt.Errorf("start uv: %w", err)
			return false
		}

		var wg sync.WaitGroup
		wg.Add(1)
		go e.StreamRunLog(&wg, pr, runID, "stderr", "warn", redactor)

		doneCh := make(chan error, 1)
		go func() {
			err := cmd.Wait()
			// The process has exited and the stderr copier has flushed to the
			// sniffer; close the pipe so the log streamer sees EOF.
			_ = pw.Close()
			doneCh <- err
		}()

		exitErr, exitedFirst := pkgruntime.AwaitBridgeCompletion(srv.ReturnCh(), doneCh, pkgruntime.BridgeShutdownGrace,
			func(retVal any) {
				result.ChainInput = retVal
				result.ReturnValue = retVal
				if out := srv.Output(); out != nil {
					result.ChainInput = out.Data
					result.OutputContentType = out.ContentType
					result.OutputContent = out.Content
				}
			},
			func() { _ = cmd.Process.Signal(syscall.SIGTERM) },
		)
		if exitedFirst {
			if out := srv.Output(); out != nil {
				result.ChainInput = out.Data
				result.OutputContentType = out.ContentType
				result.OutputContent = out.Content
			}
			if exitErr != nil {
				result.Error = exitErr
			}
		}

		wg.Wait()
		return result.Error != nil && sniffer.StaleLock()
	}

	pkgruntime.RecoverStaleLock(pkgruntime.StaleLockRecoveryEnabled(), runOnce, func() error {
		return e.relockScript(ctx, spec, runID, scriptPath)
	})

	// A legitimate dicode.suspend() exits the process cleanly (exit 0), so the
	// run is not a failure. Only then translate the captured payload into a
	// suspended result (#95). When result.Error is set the subprocess exited
	// non-zero — the wrapper's swallow-guard trips this when a task caught the
	// SuspendSignal and kept running — so keep it a failure rather than a
	// contradictory suspended-and-returned run, even though a payload was
	// recorded server-side. Mirrors the Deno runtime.
	if sr := srv.Suspend(); sr != nil && result.Error == nil {
		result.Suspended = true
		result.ResumeState = sr.State
		result.ResumeSchema = sr.Schema
		result.ResumeDeadline = sr.Deadline
	}

	return result, nil
}

// pythonRelock is the relock entry the runtime invokes; a package var so tests
// can simulate a slow or failing relock without the uv toolchain.
var pythonRelock = RelockScript

// relockScript regenerates the task.py.lock sidecar for the failing script
// after a stale-lock run failure and records an audit line with the sidecar's
// hash delta. Regenerating re-pins the dependencies the (already-approved) task
// declares, so it does not bypass the approval gate, which governs task content.
func (e *executor) relockScript(ctx context.Context, spec *task.Spec, runID, scriptPath string) error {
	sidecar := LockSidecarPath(scriptPath)
	before, _ := os.ReadFile(sidecar) //nolint:gosec
	var out strings.Builder
	if err := pythonRelock(ctx, e.uvPath, scriptPath, false, &out); err != nil {
		e.Log.Warn("stale-lock auto-recovery: uv relock failed",
			zap.String("task", spec.ID), zap.String("lock", sidecar), zap.Error(err))
		_ = e.Registry.AppendLog(context.Background(), runID, "error",
			fmt.Sprintf("auto-recovery: %s regeneration failed: %v", sidecar, err))
		return err
	}
	after, _ := os.ReadFile(sidecar) //nolint:gosec
	hb, ha := pkgruntime.ShortHash(before), pkgruntime.ShortHash(after)
	e.Log.Info("stale-lock auto-recovery: regenerated uv lock, retrying run",
		zap.String("task", spec.ID), zap.String("lock", sidecar),
		zap.String("hash_before", hb), zap.String("hash_after", ha))
	_ = e.Registry.AppendLog(context.Background(), runID, "info",
		fmt.Sprintf("auto-recovery: %s was stale, regenerated (%s→%s), retrying run", sidecar, hb, ha))
	return nil
}

// buildWrapper assembles the final Python file that uv will execute:
//
//  1. PEP 723 script block (extracted from the user script, if present) — must
//     be first so uv can parse inline dependencies.
//  2. The dicode SDK shim (dicode_sdk.py).
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
	// _dispatch auto-selects the task handler from the resume context (#512):
	// first run → main; a resume → steps[to] (wizard) or resume() (two-function),
	// falling back to main so a single-main task keeps working. The author reads
	// ctx.state / ctx.input, never a step switch. A clean dicode.suspend() raises
	// SuspendSignal after the payload is recorded — exit 0 without a return value
	// (a suspend is not a failure). If task code instead swallowed the signal and
	// returned, the suspend flag is still set: fail loudly rather than record a
	// contradictory suspended-and-returned run (#95). A suspend at task top level
	// (sync tasks) unwinds past this block and is handled by the SDK's excepthook.
	w.WriteString("try:\n")
	w.WriteString("    result = _dispatch(globals())\n")
	w.WriteString("    if _was_suspend_requested():\n")
	w.WriteString("        _sys.stderr.write(\"[dicode] dicode.suspend() was called but its control-flow signal was caught by task code — do not wrap dicode.suspend() in a try/except that swallows it\\n\")\n")
	w.WriteString("        _sys.stderr.flush()\n")
	w.WriteString("        _flush_and_close()\n")
	w.WriteString("        _sys.exit(1)\n")
	w.WriteString("    _set_return(result)\n")
	w.WriteString("except SuspendSignal:\n")
	w.WriteString("    _flush_and_close()\n")
	w.WriteString("    _sys.exit(0)\n")
	// _flush_and_close drains pending _fire writes and closes the socket on the
	// FIFO IO loop; it swallows errors so a slow close never fails a good run.
	w.WriteString("_flush_and_close()\n")
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
