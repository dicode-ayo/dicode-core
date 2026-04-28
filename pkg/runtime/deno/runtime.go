// Package deno executes task scripts using a Deno subprocess.
// Each call to Run spawns a fresh Deno process connected to a per-run
// Unix socket server that bridges globals (log, kv, params, env, input, output).
package deno

import (
	"bufio"
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dicode/dicode/pkg/db"
	denopkg "github.com/dicode/dicode/pkg/deno"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/relay"
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
}

// RunResult is returned by Run.
type RunResult struct {
	RunID       string
	ReturnValue interface{}
	Output      *ipc.OutputResult
	Logs        []*registry.LogEntry
	Error       error
}

// Runtime executes task scripts with Deno.
type Runtime struct {
	registry         *registry.Registry
	secrets          secrets.Chain
	secretsManager   secrets.Manager // optional; wired for dicode.secrets_set/delete
	db               db.DB
	log              *zap.Logger
	denoPath         string
	secret           []byte
	engine           ipc.EngineRunner
	gateway          *ipc.Gateway
	oauthIdentity    *relay.Identity
	oauthURL         string
	oauthPending     *relay.PendingSessions
	brokerPubkeyFn   func() string
	supportsOAuthFn  func() bool
	rotationActiveFn func() bool
	// secretOutputCh is opt-in: when set, every Run wires it into the
	// per-run IPC server so a provider task's dicode.output(..., {secret:
	// true}) call is routed to the resolver awaiting it. Nil leaves the
	// path inert (current behavior).
	secretOutputCh chan map[string]string
	// providerRunner is wired by the trigger engine at daemon startup so
	// the env resolver can spawn provider tasks for from: task:<id>
	// entries. Nil disables provider lookups; legacy paths still work.
	providerRunner envresolve.ProviderRunner
}

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
	return &Runtime{registry: r, secrets: sc, db: database, log: log, denoPath: path, secret: secret}, nil
}

// SetEngine configures the engine runner used for dicode.run_task calls.
func (rt *Runtime) SetEngine(e ipc.EngineRunner) { rt.engine = e }

// SetGateway attaches the HTTP gateway so daemon tasks can call http.register.
func (rt *Runtime) SetGateway(g *ipc.Gateway) { rt.gateway = g }

// SetSecretsManager wires the secrets manager so tasks with permissions.dicode.secrets_write
// can call dicode.secrets_set() and dicode.secrets_delete().
func (rt *Runtime) SetSecretsManager(m secrets.Manager) { rt.secretsManager = m }

// SetSecretOutputChannel wires the channel that receives provider tasks'
// secret maps. Called by the trigger engine before invoking Run when the
// task is being launched in "provider" mode.
func (rt *Runtime) SetSecretOutputChannel(ch chan map[string]string) {
	rt.secretOutputCh = ch
}

// SetProviderRunner wires the env-resolver's provider invocation. The
// trigger engine implements ProviderRunner and registers itself here at
// daemon startup. Nil disables provider task: lookups.
func (rt *Runtime) SetProviderRunner(p envresolve.ProviderRunner) {
	rt.providerRunner = p
}

// SetOAuthBroker wires the daemon's relay identity, broker base URL, and
// the daemon-wide PendingSessions store so the auth-start and auth-relay
// built-in tasks can use dicode.oauth.*. All three are required together;
// passing nil leaves the oauth API inert and tasks will receive a
// "not configured" error.
//
// supportsOAuthFn (issue #104) is an optional predicate reporting whether
// the currently-connected broker advertises a protocol version new enough
// to understand the split sign/decrypt key scheme. If nil the OAuth IPC
// paths are always enabled (suitable for test harnesses that don't have a
// real relay.Client). In production the daemon wires rc.SupportsOAuth so
// that an old broker cleanly disables the OAuth flows instead of silently
// failing to decrypt.
//
// rotationActiveFn (issue #144) is an optional predicate reporting whether
// a `dicode relay rotate-identity` has swapped the DB keys. The in-memory
// Identity passed here is NOT replaced by rotation; until the daemon
// restarts, signing a new auth URL under it would contradict the operator's
// stated intent. Non-nil + true refuses the OAuth IPC methods with an
// actionable error. Nil leaves the path unchecked (test fixtures).
func (rt *Runtime) SetOAuthBroker(id *relay.Identity, baseURL string, pending *relay.PendingSessions, brokerPubkeyFn func() string, supportsOAuthFn func() bool, rotationActiveFn func() bool) {
	rt.oauthIdentity = id
	rt.oauthURL = baseURL
	rt.oauthPending = pending
	rt.brokerPubkeyFn = brokerPubkeyFn
	rt.supportsOAuthFn = supportsOAuthFn
	rt.rotationActiveFn = rotationActiveFn
}

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
		runID, err = rt.registry.StartRun(ctx, spec.ID, opts.ParentRunID)
		if err != nil {
			return nil, fmt.Errorf("start run: %w", err)
		}
	}

	result := &RunResult{RunID: runID}
	status := registry.StatusSuccess

	defer func() {
		// If the run failed before Deno started (secret missing, script not found,
		// etc.) result.Error is set but no log entries exist yet. Append it now so
		// the error is visible in both the Web UI run detail and the CLI log output.
		if result.Error != nil {
			_ = rt.registry.AppendLog(context.Background(), runID, "error", result.Error.Error())
		}
		if logs, lerr := rt.registry.GetRunLogs(context.Background(), runID); lerr == nil {
			result.Logs = logs
		}
		if ferr := rt.registry.FinishRun(context.Background(), runID, status); ferr != nil {
			rt.log.Error("finish run", zap.String("run", runID), zap.Error(ferr))
		}
	}()

	// Resolve declared env permissions via the shared resolver. Provider
	// tasks (from: task:<id>) are spawned and batched at most once per
	// provider per launch; legacy paths (secret:, env:NAME, bare) are
	// preserved.
	resolvedRes, err := rt.envresolver().Resolve(ctx, spec)
	if err != nil {
		status = registry.StatusFailure
		result.Error = err
		return result, nil
	}
	resolved := resolvedRes.Env
	redactor := secrets.NewRedactor(resolvedRes.Secrets)

	taskPath := spec.ScriptPath()
	if taskPath == "" {
		status = registry.StatusFailure
		result.Error = fmt.Errorf("script not found for task %s", spec.ID)
		return result, nil
	}

	var execCtx context.Context
	var cancel context.CancelFunc
	if spec.Timeout > 0 {
		execCtx, cancel = context.WithTimeout(ctx, spec.Timeout)
	} else {
		execCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	mergedParams := mergeParams(spec.Params, opts.Params)

	srv := ipc.New(runID, spec.ID, rt.secret, rt.registry, rt.db, mergedParams, opts.Input, rt.log, spec, rt.engine)
	srv.SetGateway(rt.gateway)
	srv.SetSecrets(rt.secretsManager)
	srv.SetSecretsChain(rt.secrets)
	srv.SetRedactor(redactor)
	if rt.secretOutputCh != nil {
		srv.SetSecretOutput(rt.secretOutputCh)
	}
	if rt.oauthIdentity != nil {
		srv.SetOAuthBroker(rt.oauthIdentity, rt.oauthURL, rt.oauthPending, rt.brokerPubkeyFn, rt.supportsOAuthFn, rt.rotationActiveFn)
	}
	socketPath, token, err := srv.Start(execCtx)
	if err != nil {
		status = registry.StatusFailure
		result.Error = fmt.Errorf("start socket server: %w", err)
		return result, nil
	}
	defer srv.Stop()

	debug := rt.log.Core().Enabled(zap.DebugLevel)

	// Write the shim as a proper ES module to a temp file.
	// Run ID is embedded between the prefix and the __<random> suffix so the
	// tasks/buildin/temp-cleanup builtin can correlate orphaned files with runs.
	shimFile, err := os.CreateTemp("", "dicode-shim-"+runID+"__*.ts")
	if err != nil {
		status = registry.StatusFailure
		result.Error = fmt.Errorf("create shim file: %w", err)
		return result, nil
	}
	shimPath := shimFile.Name()
	if !debug {
		defer os.Remove(shimPath)
	}
	if _, err := shimFile.WriteString(shimContent); err != nil {
		shimFile.Close()
		status = registry.StatusFailure
		result.Error = fmt.Errorf("write shim: %w", err)
		return result, nil
	}
	shimFile.Close()

	// Write the wrapper that imports both and calls the user's exported main().
	runnerFile, err := os.CreateTemp("", "dicode-runner-"+runID+"__*.ts")
	if err != nil {
		status = registry.StatusFailure
		result.Error = fmt.Errorf("create runner file: %w", err)
		return result, nil
	}
	runnerPath := runnerFile.Name()
	if !debug {
		defer os.Remove(runnerPath)
	}
	rt.log.Debug("deno temp files",
		zap.String("task", spec.ID),
		zap.String("shim", shimPath),
		zap.String("task_script", taskPath),
		zap.String("runner", runnerPath),
	)
	runner := "import { params, kv, input, output, mcp, dicode, __setReturn__, __conn__, __flush__ } from \"" + shimPath + "\";\n" +
		"let __main__;\n" +
		"try {\n" +
		"  __main__ = (await import(\"" + taskPath + "\")).default;\n" +
		"} catch (__importErr__) {\n" +
		"  console.error(\"[dicode] task import failed:\", String(__importErr__));\n" +
		"  await __flush__();\n" +
		"  try { __conn__.close(); } catch {}\n" +
		"  Deno.exit(1);\n" +
		"}\n" +
		"try {\n" +
		"  const result = await __main__({ params, kv, input, output, mcp, dicode });\n" +
		"  await __setReturn__(result);\n" +
		"} finally {\n" +
		"  await __flush__();\n" +
		"  try { __conn__.close(); } catch {}\n" +
		"}\n"
	if _, err := runnerFile.WriteString(runner); err != nil {
		runnerFile.Close()
		status = registry.StatusFailure
		result.Error = fmt.Errorf("write runner: %w", err)
		return result, nil
	}
	runnerFile.Close()

	args := buildDenoArgs(spec, socketPath, shimPath, runnerPath, rt.oauthURL)
	cmd := exec.CommandContext(execCtx, rt.denoPath, args...) //nolint:gosec
	cmd.Env = buildEnv(resolved, socketPath, token, rt.oauthURL)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		status = registry.StatusFailure
		result.Error = err
		return result, nil
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		status = registry.StatusFailure
		result.Error = err
		return result, nil
	}

	if err := cmd.Start(); err != nil {
		status = registry.StatusFailure
		result.Error = fmt.Errorf("start deno: %w", err)
		return result, nil
	}

	// Register PID so metrics can aggregate child process resource usage.
	pid := cmd.Process.Pid
	activePIDs.Store(pid, struct{}{})
	defer activePIDs.Delete(pid)

	// Stream stdout (console.log/info) as "info" and stderr (console.error +
	// Deno runtime errors) as "error" in the run log.
	// wg ensures all log lines are flushed before Run returns, avoiding the race
	// where the caller fetches logs immediately after exit and sees an empty list.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			_ = rt.registry.AppendLog(context.Background(), runID, "info", redactor.RedactString(scanner.Text()))
		}
	}()
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			_ = rt.registry.AppendLog(context.Background(), runID, "error", redactor.RedactString(scanner.Text()))
		}
	}()

	doneCh := make(chan error, 1)
	go func() { doneCh <- cmd.Wait() }()

	select {
	case retVal := <-srv.ReturnCh():
		result.ReturnValue = retVal
		result.Output = srv.Output()
		// Process exits shortly after posting /return; give it a moment.
		select {
		case <-doneCh:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}

	case exitErr := <-doneCh:
		// Check for a return value that arrived just before exit (non-blocking).
		select {
		case retVal := <-srv.ReturnCh():
			result.ReturnValue = retVal
		default:
		}
		result.Output = srv.Output()
		if exitErr != nil {
			if execCtx.Err() != nil {
				status = registry.StatusCancelled
			} else {
				status = registry.StatusFailure
			}
			result.Error = exitErr
		}
	}

	// Wait for stdout/stderr scanners to flush all log lines before returning.
	// Without this, callers that fetch logs immediately after Run returns may see
	// an empty list because the goroutines haven't written to the DB yet.
	wg.Wait()

	return result, nil
}

func mergeParams(specParams []task.Param, overrides map[string]string) map[string]string {
	out := make(map[string]string, len(specParams))
	for _, p := range specParams {
		if p.Default != "" {
			out[p.Name] = p.Default
		}
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
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

func buildDenoArgs(spec *task.Spec, socketPath, shimPath, runnerPath, brokerURL string) []string {
	args := []string{"run"}

	// Network: omit = unrestricted (--allow-net); empty list = deny all; named hosts = allowlist.
	// The IPC socket itself uses a Unix socket (--allow-read/write), not TCP, so net
	// permission does not affect it.
	net := spec.Permissions.Net
	if net == nil {
		// no net: field → unrestricted (backward-compatible default)
		args = append(args, "--allow-net")
	} else if len(net) == 1 && net[0] == "*" {
		args = append(args, "--allow-net")
	} else if len(net) > 0 {
		args = append(args, "--allow-net="+strings.Join(net, ","))
	}
	// len(net) == 0 (explicit empty list) → no --allow-net flag → network denied

	// Env: always allow the internal IPC vars plus HOME/DENO_DIR/XDG_CACHE_HOME
	// (required by deno.land/x/cache for vendored binary downloads).
	// DICODE_BROKER_URL (issue #84) is daemon-provided infrastructure —
	// auto-allowed when a broker URL is configured so auth tasks don't
	// need to redeclare it in permissions.env.
	envVars := []string{"DICODE_SOCKET", "DICODE_TOKEN", "HOME", "DENO_DIR", "XDG_CACHE_HOME"}
	if brokerURL != "" {
		envVars = append(envVars, "DICODE_BROKER_URL")
	}
	for _, e := range spec.Permissions.Env {
		envVars = append(envVars, e.Name)
	}
	args = append(args, "--allow-env="+strings.Join(envVars, ","))

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

	run := spec.Permissions.Run
	if len(run) == 1 && run[0] == "*" {
		args = append(args, "--allow-run")
	} else if len(run) > 0 {
		args = append(args, "--allow-run="+strings.Join(run, ","))
	}

	args = append(args, runnerPath)
	return args
}

func buildEnv(resolved map[string]string, socketPath, token, brokerURL string) []string {
	// Inherit the host environment so Deno can locate its cache (DENO_DIR etc).
	// The --allow-env flag separately controls which vars the JS script can read.
	env := append(os.Environ(), "DICODE_SOCKET="+socketPath, "DICODE_TOKEN="+token)
	// DICODE_BROKER_URL (issue #84) — daemon-resolved OAuth broker base URL.
	// Injected only when configured so tasks can distinguish "broker disabled"
	// from "broker at the default".
	if brokerURL != "" {
		env = append(env, "DICODE_BROKER_URL="+brokerURL)
	}
	for k, v := range resolved {
		env = append(env, k+"="+v)
	}
	return env
}

// envresolver lazily constructs the env resolver. Wired with the daemon's
// secret chain + a provider runner that calls back into the trigger
// engine. Nil engine (test harness with no providers) yields a resolver
// that errors on any from: task:<id> entry.
func (rt *Runtime) envresolver() *envresolve.Resolver {
	return envresolve.New(rt.registry, rt.secrets, rt.providerRunner)
}

// Execute implements runtime.Executor.
func (rt *Runtime) Execute(ctx context.Context, spec *task.Spec, opts pkgruntime.RunOptions) (*pkgruntime.RunResult, error) {
	result, err := rt.Run(ctx, spec, RunOptions{
		RunID:       opts.RunID,
		ParentRunID: opts.ParentRunID,
		Params:      opts.Params,
		Input:       opts.Input,
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
	return r, nil
}
