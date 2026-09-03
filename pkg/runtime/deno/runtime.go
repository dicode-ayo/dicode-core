// Package deno executes task scripts using a Deno subprocess.
// Each call to Run spawns a fresh Deno process connected to a per-run
// Unix socket server that bridges globals (log, kv, params, env, input, output).
package deno

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/dicode/dicode/internal/fsutil"
	"github.com/dicode/dicode/pkg/db"
	denopkg "github.com/dicode/dicode/pkg/deno"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/runtime/envresolve"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// activePIDs tracks PIDs of all currently running Deno subprocesses.
var activePIDs sync.Map // map[int]struct{}

// ActivePIDs returns the PIDs of all currently running Deno subprocesses.
func ActivePIDs() []int {
	var pids []int
	activePIDs.Range(func(k, _ any) bool {
		pids = append(pids, k.(int))
		return true
	})
	return pids
}

//go:embed sdk/shim.ts
var shimContent string

// SdkDts is the TypeScript declaration file for the dicode task SDK.
// Exposed for use by the web UI to provide Monaco IntelliSense.
//
//go:embed sdk/sdk.d.ts
var SdkDts []byte

// RunOptions controls a single task execution.
type RunOptions struct {
	RunID       string
	Params      map[string]string
	Input       interface{}
	ParentRunID string

	// PreResolvedEnv, when set, is the result of an env-resolver pass run
	// by the trigger engine before dispatch (issue #235). Run uses these
	// values directly instead of constructing its own resolver. Nil falls
	// back to the inline resolver path.
	PreResolvedEnv *envresolve.Resolved

	// ResumeState / ResumeInput carry a suspended run's prior state and the
	// user's form submission when this run is a resume (#95). Both are opaque
	// JSON blobs; the SDK unwraps the step envelope and surfaces them to the
	// task as ctx.state / ctx.input. Nil on a first (non-resume) invocation.
	ResumeState json.RawMessage
	ResumeInput json.RawMessage

	// Resumed marks this run as a resume continuation. It is the resume signal
	// the SDK dispatches on; carried state may legitimately be null.
	Resumed bool

	// SecretOutputCh, when set, is wired into this run's IPC server so a
	// provider task's dicode.output(map, { secret: true }) call routes to
	// the caller awaiting it. Forwarded from the generic
	// pkgruntime.RunOptions by Execute; nil for every non-provider run
	// (issue #719 — see that field's doc for why this replaced shared
	// BridgeDeps.SecretOutputCh state).
	SecretOutputCh chan map[string]string
}

// RunResult is returned by Run.
type RunResult struct {
	RunID       string
	ReturnValue interface{}
	Output      *ipc.OutputResult
	Logs        []*registry.LogEntry
	Error       error

	// Suspended is set when the task called dicode.suspend() (#95): the run
	// paused cleanly rather than completing or failing. ResumeState/ResumeSchema
	// are the opaque state blob and form schema; ResumeDeadline is an optional
	// Unix-ms TTL (0 = unset).
	Suspended      bool
	ResumeState    []byte
	ResumeSchema   []byte
	ResumeDeadline int64
}

// Runtime executes task scripts with Deno.
type Runtime struct {
	// parent is non-nil when this Runtime was created by NewExecutor (i.e. it
	// is acting as a per-version executor rather than the manager-owned
	// instance). live() reads from parent.BridgeDeps so that late Set* calls
	// on the manager (SetInputStore, SetReplayer, ...) propagate to all
	// executors without extra bookkeeping. Nil means "I am the manager; use
	// my own fields directly."
	parent *Runtime

	// BridgeDeps carries the dependency-injection surface shared with the
	// Python runtime — registry/secrets/IPC wiring plus the promoted Set*
	// methods daemon.go calls at boot. See pkg/runtime/bridgedeps.go.
	pkgruntime.BridgeDeps

	denoPath string

	// cryptoDeriver enables dicode.crypto.{encrypt, decrypt} for tasks that
	// declare permissions.dicode.crypto. Wired at daemon boot via
	// SetCryptoHandler; reads through parent in per-version executors.
	// Deno-only: the Python runtime has no crypto IPC surface, so this field
	// stays here rather than in the shared BridgeDeps.
	cryptoDeriver ipc.SubKeyDeriver // optional; nil disables crypto IPC
}

// live returns the BridgeDeps to consult for late-wired capabilities
// (InputStore, Replayer, SourceMgr, RepoResolver, TestGuard,
// ProtectedPaths): the parent's when this Runtime is a per-version executor,
// its own otherwise. This is what makes a daemon-level Set* call that runs
// after NewExecutor still visible to every executor.
func (rt *Runtime) live() *pkgruntime.BridgeDeps {
	if rt.parent != nil {
		return &rt.parent.BridgeDeps
	}
	return &rt.BridgeDeps
}

// effectiveInputStore returns the live InputStore to use for this runtime
// instance. When this Runtime is a per-version executor (parent != nil) it
// reads from the parent so that a daemon-level SetInputStore call that runs
// after NewExecutor is still visible here.
func (rt *Runtime) effectiveInputStore() *registry.InputStore { return rt.live().InputStore }

// effectiveReplayer returns the live Replayer, reading from parent when this
// is a per-version executor so that a late SetReplayer call on the manager
// propagates without extra bookkeeping.
func (rt *Runtime) effectiveReplayer() *registry.Replayer { return rt.live().Replayer }

// effectiveSourceMgr returns the live SourceController, reading from parent
// when this is a per-version executor.
func (rt *Runtime) effectiveSourceMgr() ipc.SourceController { return rt.live().SourceMgr }

// effectiveRepoResolver returns the live RepoPathResolver, reading from parent
// when this is a per-version executor.
func (rt *Runtime) effectiveRepoResolver() ipc.RepoPathResolver { return rt.live().RepoResolver }

// New creates a Deno Runtime. It ensures the Deno binary is present in the
// cache, downloading it if necessary.
func New(r *registry.Registry, sc secrets.Chain, database db.DB, log *zap.Logger) (*Runtime, error) {
	path, err := denopkg.EnsureDeno(denopkg.DefaultVersion)
	if err != nil {
		return nil, fmt.Errorf("ensure deno: %w", err)
	}
	secret, err := ipc.NewSecret()
	if err != nil {
		return nil, fmt.Errorf("ipc secret: %w", err)
	}
	return &Runtime{
		BridgeDeps: pkgruntime.BridgeDeps{Registry: r, SecretsChain: sc, DB: database, Log: log, IPCSecret: secret},
		denoPath:   path,
	}, nil
}

// The Set* dependency wiring (SetEngine, SetGateway, SetInputStore, ...) is
// promoted from the embedded pkgruntime.BridgeDeps — see bridgedeps.go for
// the full surface shared with the Python runtime.

// SetCryptoHandler wires the SubKeyDeriver so per-run IPC servers can serve
// dicode.crypto.{encrypt, decrypt} calls. Must be called before any Run;
// mirrors the SetEngine / SetGateway pattern. Deno-only — the Python runtime
// has no crypto IPC surface.
func (rt *Runtime) SetCryptoHandler(d ipc.SubKeyDeriver) { rt.cryptoDeriver = d }

// effectiveCryptoDeriver returns the live SubKeyDeriver, reading through
// parent when this is a per-version executor so a daemon-level
// SetCryptoHandler call propagates without extra bookkeeping.
func (rt *Runtime) effectiveCryptoDeriver() ipc.SubKeyDeriver {
	if rt.parent != nil {
		return rt.parent.cryptoDeriver
	}
	return rt.cryptoDeriver
}

// effectiveProtectedPaths returns the protected paths, reading through parent
// when this is a per-version executor so a daemon-level SetProtectedPaths call
// propagates without extra bookkeeping. Every Run emits each as a
// --deny-write so the deny takes precedence over any --allow-write.
func (rt *Runtime) effectiveProtectedPaths() []string { return rt.live().ProtectedPaths }

// Run executes a task script and returns the result.
func (rt *Runtime) Run(ctx context.Context, spec *task.Spec, opts RunOptions) (*RunResult, error) {
	if opts.Params == nil {
		opts.Params = map[string]string{}
	}

	var runID string
	var err error
	if opts.RunID != "" {
		runID = opts.RunID
	} else {
		runID, err = rt.Registry.StartRun(ctx, spec.ID, opts.ParentRunID)
		if err != nil {
			return nil, fmt.Errorf("start run: %w", err)
		}
	}

	result := &RunResult{RunID: runID}

	defer func() {
		// If the run failed before Deno started (secret missing, script not found,
		// etc.) result.Error is set but no log entries exist yet. Append it now so
		// the error is visible in both the Web UI run detail and the CLI log output.
		if result.Error != nil {
			_ = rt.Registry.AppendLog(context.Background(), runID, "error", result.Error.Error())
		}
		if logs, lerr := rt.Registry.GetRunLogs(context.Background(), runID); lerr == nil {
			result.Logs = logs
		}
	}()

	// Resolve declared env permissions — preferring the trigger engine's
	// preflight result over inline resolution (see ResolveRunEnv).
	resolved, redactor, err := pkgruntime.ResolveRunEnv(ctx, spec, opts.PreResolvedEnv, rt.envresolver)
	if err != nil {
		result.Error = err
		return result, nil
	}

	// Ephemeral per-run MCP token (opt-in via permissions.env
	// DICODE_MCP_API_KEY): mint before anything else touches resolved, and
	// defer the revoke unconditionally so it fires on every exit path below.
	mcpToken, mcpRevoke, err := pkgruntime.ApplyMCPToken(ctx, rt.live(), rt.Log, spec, runID, resolved)
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

	taskPath := spec.ScriptPath()
	if taskPath == "" {
		result.Error = fmt.Errorf("script not found for task %s", spec.ID)
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

	srv := rt.BridgeDeps.NewIPCServer(runID, spec, mergedParams, opts.Input, redactor, rt.live())
	// Per-run provider secret-output channel (issue #719): set only on the
	// one run the trigger engine is actually firing as a provider.
	if opts.SecretOutputCh != nil {
		srv.SetSecretOutput(opts.SecretOutputCh)
	}
	// Crypto IPC is Deno-only; wired here rather than in the shared helper.
	if d := rt.effectiveCryptoDeriver(); d != nil {
		srv.SetCryptoHandler(d)
	}
	// Inject a resumed run's prior state + user input (#95); nil on first run.
	srv.SetResume(opts.Resumed, opts.ResumeState, opts.ResumeInput)
	socketPath, token, err := srv.Start(srvCtx)
	if err != nil {
		result.Error = fmt.Errorf("start socket server: %w", err)
		return result, nil
	}
	defer srv.Stop()

	debug := rt.Log.Core().Enabled(zap.DebugLevel)

	// Write the shim as a proper ES module to a temp file.
	// Run ID is embedded between the prefix and the __<random> suffix so the
	// buildin temp-cleanup task can correlate orphaned files with runs.
	shimFile, err := os.CreateTemp("", "dicode-shim-"+runID+"__*.ts")
	if err != nil {
		result.Error = fmt.Errorf("create shim file: %w", err)
		return result, nil
	}
	shimPath := shimFile.Name()
	if !debug {
		defer os.Remove(shimPath)
	}
	if _, err := shimFile.WriteString(shimContent); err != nil {
		shimFile.Close()
		result.Error = fmt.Errorf("write shim: %w", err)
		return result, nil
	}
	shimFile.Close()

	// Write the wrapper that imports both and calls the user's exported main().
	runnerFile, err := os.CreateTemp("", "dicode-runner-"+runID+"__*.ts")
	if err != nil {
		result.Error = fmt.Errorf("create runner file: %w", err)
		return result, nil
	}
	runnerPath := runnerFile.Name()
	if !debug {
		defer os.Remove(runnerPath)
	}
	rt.Log.Debug("deno temp files",
		zap.String("task", spec.ID),
		zap.String("shim", shimPath),
		zap.String("task_script", taskPath),
		zap.String("runner", runnerPath),
	)
	runner := "import { params, kv, input, state, output, mcp, dicode, __setReturn__, __conn__, __flush__, __isSuspend__, __wasSuspendRequested__, __resumed__, __resumeStep__ } from \"" + shimPath + "\";\n" +
		"let __mod__;\n" +
		"try {\n" +
		"  __mod__ = await import(\"" + taskPath + "\");\n" +
		"} catch (__importErr__) {\n" +
		"  console.error(\"[dicode] task import failed:\", String(__importErr__));\n" +
		"  await __flush__();\n" +
		"  try { __conn__.close(); } catch {}\n" +
		"  Deno.exit(1);\n" +
		"}\n" +
		// Auto-dispatch (#512): a first run calls main; a resume runs steps[to]
		// (wizard shape) when the marker names an exported step, else the exported
		// resume (two-function shape), else falls back to main so a single-main
		// task keeps working. The author reads ctx.state / ctx.input, never a step
		// switch. main is the entry (first) step. When `steps` is exported but the
		// marker names no matching step (typo, or the task was edited mid-wizard),
		// fail loudly rather than silently re-running an unrelated handler against
		// mid-wizard state.
		"const __ctx__ = { params, kv, input, state, output, mcp, dicode };\n" +
		"const __main__ = __mod__.default;\n" +
		"const __resumeFn__ = __mod__.resume;\n" +
		"const __steps__ = __mod__.steps;\n" +
		"let __handler__ = __main__;\n" +
		"if (__resumed__) {\n" +
		"  if (__steps__ && __resumeStep__) {\n" +
		"    if (typeof __steps__[__resumeStep__] === \"function\") { __handler__ = __steps__[__resumeStep__]; }\n" +
		"    else {\n" +
		"      console.error(\"[dicode] resume step \\\"\" + __resumeStep__ + \"\\\" is not an exported step function\");\n" +
		"      await __flush__();\n" +
		"      try { __conn__.close(); } catch {}\n" +
		"      Deno.exit(1);\n" +
		"    }\n" +
		"  }\n" +
		"  else if (typeof __resumeFn__ === \"function\") { __handler__ = __resumeFn__; }\n" +
		"}\n" +
		"if (typeof __handler__ !== \"function\") {\n" +
		"  console.error(\"[dicode] task has no default export (main) to run\");\n" +
		"  await __flush__();\n" +
		"  try { __conn__.close(); } catch {}\n" +
		"  Deno.exit(1);\n" +
		"}\n" +
		"try {\n" +
		"  const result = await __handler__(__ctx__);\n" +
		// If main() returned normally yet a suspend was requested, the task
		// caught the SuspendSignal in its own try/catch and kept running. The
		// payload is already recorded server-side, so a normal return would
		// leave the run in a contradictory suspended-and-returned state. Fail
		// loudly (exit 1) instead so the author fixes the swallowing catch.
		"  if (__wasSuspendRequested__()) {\n" +
		"    console.error(\"[dicode] dicode.suspend() was called but its control-flow signal was caught by task code — do not wrap dicode.suspend() in a try/catch that swallows it\");\n" +
		"    await __flush__();\n" +
		"    try { __conn__.close(); } catch {}\n" +
		"    Deno.exit(1);\n" +
		"  }\n" +
		"  await __setReturn__(result);\n" +
		"} catch (__err__) {\n" +
		// A dicode.suspend() throws SuspendSignal after the payload is already
		// delivered over IPC. Treat it as a clean exit (0), not a failure.
		"  if (__isSuspend__(__err__)) {\n" +
		"    await __flush__();\n" +
		"    try { __conn__.close(); } catch {}\n" +
		"    Deno.exit(0);\n" +
		"  }\n" +
		"  throw __err__;\n" +
		"} finally {\n" +
		"  await __flush__();\n" +
		"  try { __conn__.close(); } catch {}\n" +
		"}\n"
	if _, err := runnerFile.WriteString(runner); err != nil {
		runnerFile.Close()
		result.Error = fmt.Errorf("write runner: %w", err)
		return result, nil
	}
	runnerFile.Close()

	args := buildDenoArgs(spec, socketPath, shimPath, runnerPath, rt.effectiveProtectedPaths())

	// runOnce spawns the Deno subprocess, streams its output, and records the
	// outcome on result. It reports whether the run failed with the stale-lock
	// signature so a single deterministic relock+retry can recover a drifted
	// deno.lock without invoking the AI auto-fix loop (issue #455).
	runOnce := func() (staleLock bool) {
		result.Error = nil
		result.Output = nil
		result.ReturnValue = nil

		// Fresh per-attempt timeout budget: the retry's deadline is measured
		// from here (after any relock), so a slow relock cannot starve it.
		execCtx, cancel := pkgruntime.ExecContext(ctx, spec.Timeout)
		defer cancel()

		cmd := exec.CommandContext(execCtx, rt.denoPath, args...) //nolint:gosec
		cmd.Env = pkgruntime.SubprocessEnv(spec, resolved, socketPath, token)
		sniffer := pkgruntime.NewLockErrSniffer(staleLockSignature)

		var wg sync.WaitGroup
		// stderrW, when non-nil, is closed by the cmd.Wait goroutine once the
		// process has exited and every stderr byte has been copied to the
		// sniffer — this is what lets the log streamer drain to EOF.
		var stderrW *io.PipeWriter
		if spec.Silent {
			// Discard stdout — no AppendLog calls — but still sniff stderr for the
			// stale-lock signature so recovery works for silent tasks too.
			cmd.Stdout = io.Discard
			cmd.Stderr = sniffer
			if err := cmd.Start(); err != nil {
				result.Error = fmt.Errorf("start deno: %w", err)
				return false
			}
		} else {
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				result.Error = err
				return false
			}
			// Route stderr through cmd.Stderr (a writer), not StderrPipe: this
			// makes cmd.Wait block until every stderr byte has been copied to the
			// sniffer, so the stale-lock decision never races the process exit
			// closing a pipe. The bytes are teed to a pipe that feeds the log
			// streamer, so they still reach the run log ("error" level).
			pr, pw := io.Pipe()
			stderrW = pw
			cmd.Stderr = io.MultiWriter(sniffer, pw)
			if err := cmd.Start(); err != nil {
				result.Error = fmt.Errorf("start deno: %w", err)
				return false
			}
			// wg ensures all log lines are flushed before Run returns, avoiding the race
			// where the caller fetches logs immediately after exit and sees an empty list.
			wg.Add(2)
			go rt.StreamRunLog(&wg, stdout, runID, "stdout", "info", redactor)
			go rt.StreamRunLog(&wg, pr, runID, "stderr", "error", redactor)
		}

		// Register PID so metrics can aggregate child process resource usage.
		pid := cmd.Process.Pid
		activePIDs.Store(pid, struct{}{})

		doneCh := make(chan error, 1)
		go func() {
			err := cmd.Wait()
			// The process has exited and (for the non-silent path) the stderr
			// copier has flushed to the sniffer; close the pipe so the log
			// streamer sees EOF and wg can complete.
			if stderrW != nil {
				_ = stderrW.Close()
			}
			doneCh <- err
		}()

		exitErr, exitedFirst := pkgruntime.AwaitBridgeCompletion(srv.ReturnCh(), doneCh, pkgruntime.BridgeShutdownGrace,
			func(retVal any) {
				result.ReturnValue = retVal
				result.Output = srv.Output()
			},
			func() { _ = cmd.Process.Signal(syscall.SIGTERM) },
		)
		if exitedFirst {
			result.Output = srv.Output()
			if exitErr != nil {
				result.Error = exitErr
			}
		}

		activePIDs.Delete(pid)
		// Wait for stdout/stderr scanners to flush all log lines before returning.
		// Without this, callers that fetch logs immediately after Run returns may see
		// an empty list because the goroutines haven't written to the DB yet.
		wg.Wait()

		return result.Error != nil && sniffer.StaleLock()
	}

	// A deno.lock is only enforced (and only stale-fails) when one is present at
	// or near the task dir; recover only then, and only when not opted out.
	lockPath := denoLockForRecovery(spec)
	enabled := lockPath != "" && pkgruntime.StaleLockRecoveryEnabled()
	pkgruntime.RecoverStaleLock(enabled, runOnce, func() error {
		return rt.relockDeno(ctx, spec, runID, lockPath)
	})

	// A legitimate dicode.suspend() exits the process cleanly (exit 0), so the
	// run is not a failure. Only then translate the captured payload into a
	// suspended result (#95). When result.Error is set the subprocess exited
	// non-zero — the shim's guard trips this path when a task swallowed the
	// SuspendSignal and kept running — so keep it a failure rather than a
	// contradictory suspended-and-returned run, even though a payload was
	// recorded server-side.
	if sr := srv.Suspend(); sr != nil && result.Error == nil {
		result.Suspended = true
		result.ResumeState = sr.State
		result.ResumeSchema = sr.Schema
		result.ResumeDeadline = sr.Deadline
	}

	return result, nil
}

// denoLockForRecovery returns the deno.lock path enforced for spec (the one a
// stale-lock failure would come from), or "" when the task opts out via its own
// deno.json or has no lock nearby. Mirrors the enforcement decision in
// buildDenoArgs.
func denoLockForRecovery(spec *task.Spec) string {
	if _, err := os.Stat(filepath.Join(spec.TaskDir, "deno.json")); !os.IsNotExist(err) {
		return ""
	}
	return findDenoLockFile(spec.TaskDir, 2)
}

// denoRelock is the relock entry the runtime invokes; a package var so tests
// can simulate a slow or failing relock without the Deno toolchain.
var denoRelock = Relock

// relockDeno regenerates the shared deno.lock covering spec's task tree after a
// stale-lock run failure and records an audit line with the lock's hash delta.
// Regenerating re-pins the dependencies the (already-approved) task declares,
// so it does not bypass the approval gate, which governs task content.
func (rt *Runtime) relockDeno(ctx context.Context, spec *task.Spec, runID, lockPath string) error {
	before, _ := os.ReadFile(lockPath) //nolint:gosec
	dir := filepath.Dir(lockPath)
	var out strings.Builder
	if _, err := denoRelock(ctx, dir, false, &out); err != nil {
		rt.Log.Warn("stale-lock auto-recovery: deno relock failed",
			zap.String("task", spec.ID), zap.String("lock", lockPath), zap.Error(err))
		_ = rt.Registry.AppendLog(context.Background(), runID, "error",
			fmt.Sprintf("auto-recovery: deno.lock regeneration failed: %v", err))
		return err
	}
	after, _ := os.ReadFile(lockPath) //nolint:gosec
	hb, ha := pkgruntime.ShortHash(before), pkgruntime.ShortHash(after)
	rt.Log.Info("stale-lock auto-recovery: regenerated deno.lock, retrying run",
		zap.String("task", spec.ID), zap.String("lock", lockPath),
		zap.String("hash_before", hb), zap.String("hash_after", ha))
	_ = rt.Registry.AppendLog(context.Background(), runID, "info",
		fmt.Sprintf("auto-recovery: deno.lock was stale, regenerated (%s→%s), retrying run", hb, ha))
	return nil
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") || p == "~" {
		home, err := os.UserHomeDir()
		if err == nil {
			return home + p[1:]
		}
	}
	return p
}

// findDenoLockFile walks up from dir (at most maxParents levels) looking for
// a deno.lock file. Returns the absolute path on the first match, or "".
// maxParents=2 covers a taskset laid out as <set>/<name>/ → <set>/../deno.lock.
func findDenoLockFile(dir string, maxParents int) string {
	path, _ := fsutil.FindUp(dir, "deno.lock", maxParents)
	return path
}

func buildDenoArgs(spec *task.Spec, socketPath, shimPath, runnerPath string, protectedPaths []string) []string {
	args := []string{"run"}

	// Enforce the lockfile when a deno.lock is found at or near the task directory.
	// Skip when the task has its own deno.json: that file controls lock configuration
	// (including "lock": false opt-out) and Deno respects it without our help.
	// maxParents=2 covers <set>/<name>/ → <set>/../deno.lock.
	if _, err := os.Stat(filepath.Join(spec.TaskDir, "deno.json")); os.IsNotExist(err) {
		if lf := findDenoLockFile(spec.TaskDir, 2); lf != "" {
			args = append(args, "--lock="+lf, "--frozen")
		}
	}

	// Network is deny-by-default: omit or [] = deny all; ["*"] = unrestricted;
	// named hosts = allowlist. The IPC socket itself uses a Unix socket
	// (--allow-read/write), not TCP, so net permission does not affect it.
	net := spec.Permissions.Net
	if len(net) == 1 && net[0] == "*" {
		args = append(args, "--allow-net")
	} else if len(net) > 0 {
		args = append(args, "--allow-net="+strings.Join(net, ","))
	}
	// nil or explicit empty list → no --allow-net flag → network denied

	// EnvReadExposed grants bare --allow-env (read any var). The blast radius is
	// bounded by runtime.SubprocessEnv, which forwards only an allowlist
	// (PATH/HOME/cache/proxy/TLS vars, DICODE_SOCKET/DICODE_TOKEN, and the
	// task's own resolved vars) and denylists the daemon's master/admin keys,
	// so "read any var" can only read what the task already holds. Needed for
	// Deno node-compat / npm tasks whose transitive deps read unpredictable
	// process.env keys at module init. Otherwise the explicit list: the
	// internal IPC vars plus HOME/DENO_DIR/XDG_CACHE_HOME (required by
	// deno.land/x/cache for vendored binary downloads) plus declared names.
	if spec.Permissions.EnvReadExposed {
		args = append(args, "--allow-env")
	} else {
		envVars := []string{"DICODE_SOCKET", "DICODE_TOKEN", "HOME", "DENO_DIR", "XDG_CACHE_HOME"}
		for _, e := range spec.Permissions.Env {
			// A pattern entry's literal name ("GITHUB_*") is not a readable var;
			// its expanded matches are appended below so they reach the sandbox.
			if pkgruntime.IsWildcardEnvEntry(e) {
				continue
			}
			envVars = append(envVars, e.Name)
		}
		envVars = append(envVars, pkgruntime.WildcardEnvNames(spec)...)
		args = append(args, "--allow-env="+strings.Join(envVars, ","))
	}

	// Sys: omit field = deny all (default); ["*"] = all; named = allowlist.
	sys := spec.Permissions.Sys
	if len(sys) == 1 && sys[0] == "*" {
		args = append(args, "--allow-sys")
	} else if len(sys) > 0 {
		args = append(args, "--allow-sys="+strings.Join(sys, ","))
	}

	// Deno 2.x requires explicit read+write permission for Unix socket paths.
	// The shim needs read permission since it is imported. The entire task
	// directory is allowed so helper modules (e.g. ./lib/foo.ts) can be imported.
	readPaths := []string{socketPath, shimPath, spec.TaskDir}
	writePaths := []string{socketPath}
	for _, entry := range spec.Permissions.FS {
		path := expandHome(entry.Path)
		if !filepath.IsAbs(path) {
			path = filepath.Join(spec.TaskDir, path)
		}
		// Clean every declared path so "../" segments and redundant separators
		// resolve before they reach Deno, keeping the grant set canonical.
		path = filepath.Clean(path)
		switch entry.Permission {
		case "r":
			readPaths = append(readPaths, path)
		case "w":
			writePaths = append(writePaths, path)
		case "rw":
			readPaths = append(readPaths, path)
			writePaths = append(writePaths, path)
		}
	}
	args = append(args, "--allow-read="+strings.Join(readPaths, ","))
	args = append(args, "--allow-write="+strings.Join(writePaths, ","))
	// The approval-gate state (dicode.lock, dicode.yaml) must never be writable
	// by a task: a broad --allow-write covering the config dir would otherwise
	// let a task overwrite the lock to self-approve. --deny-write takes
	// precedence over any allow, so emit it unconditionally.
	if len(protectedPaths) > 0 {
		args = append(args, "--deny-write="+strings.Join(protectedPaths, ","))
	}

	run := spec.Permissions.Run
	if len(run) == 1 && run[0] == "*" {
		args = append(args, "--allow-run")
	} else if len(run) > 0 {
		args = append(args, "--allow-run="+strings.Join(run, ","))
	}

	args = append(args, runnerPath)
	return args
}

// envresolver returns the env resolver to use for a Run, reading the
// daemon-scoped shared resolver (issue #242) through parent for per-version
// executors. See pkgruntime.LiveResolver for the precedence order.
func (rt *Runtime) envresolver() *envresolve.Resolver {
	var parent *pkgruntime.BridgeDeps
	if rt.parent != nil {
		parent = &rt.parent.BridgeDeps
	}
	return pkgruntime.LiveResolver(&rt.BridgeDeps, parent)
}

// Execute implements runtime.Executor.
func (rt *Runtime) Execute(ctx context.Context, spec *task.Spec, opts pkgruntime.RunOptions) (*pkgruntime.RunResult, error) {
	result, err := rt.Run(ctx, spec, RunOptions{
		RunID:          opts.RunID,
		ParentRunID:    opts.ParentRunID,
		Params:         opts.Params,
		Input:          opts.Input,
		PreResolvedEnv: opts.PreResolvedEnv,
		Resumed:        opts.Resumed,
		ResumeState:    opts.ResumeState,
		ResumeInput:    opts.ResumeInput,
		SecretOutputCh: opts.SecretOutputCh,
	})
	if err != nil {
		return nil, err
	}
	r := &pkgruntime.RunResult{RunID: result.RunID, Error: result.Error, ReturnValue: result.ReturnValue}
	if result.Output != nil {
		r.OutputContentType = result.Output.ContentType
		r.OutputContent = result.Output.Content
		r.ChainInput = result.Output.Data
	} else {
		r.ChainInput = result.ReturnValue
	}
	if result.Suspended {
		r.Suspended = true
		r.ResumeState = result.ResumeState
		r.ResumeSchema = result.ResumeSchema
		r.ResumeDeadline = result.ResumeDeadline
	}
	return r, nil
}
