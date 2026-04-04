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
	"strings"
	"syscall"
	"time"

	"github.com/dicode/dicode/pkg/db"
	denopkg "github.com/dicode/dicode/pkg/deno"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

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
	registry       *registry.Registry
	secrets        secrets.Chain
	secretsManager secrets.Manager // optional; wired for dicode.secrets_set/delete
	db             db.DB
	log            *zap.Logger
	denoPath       string
	secret         []byte
	engine         ipc.EngineRunner
	gateway        *ipc.Gateway
	aiBaseURL      string
	aiModel        string
	aiAPIKey       string
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

// SetAIConfig configures the AI provider details passed to tasks via dicode.get_config.
func (rt *Runtime) SetAIConfig(baseURL, model, apiKey string) {
	rt.aiBaseURL = baseURL
	rt.aiModel = model
	rt.aiAPIKey = apiKey
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
		if logs, lerr := rt.registry.GetRunLogs(context.Background(), runID); lerr == nil {
			result.Logs = logs
		}
		if ferr := rt.registry.FinishRun(context.Background(), runID, status); ferr != nil {
			rt.log.Error("finish run", zap.String("run", runID), zap.Error(ferr))
		}
	}()

	// Resolve only env vars explicitly declared in permissions.env.
	// - entry.Value  → literal (taskset override); inject directly
	// - entry.Secret → look up in secrets store; fail if not found
	// - entry.From   → read from host OS env (os.Getenv); inject as entry.Name
	// - bare name    → allowlisted via --allow-env; script reads from host env at runtime
	resolved := make(map[string]string, len(spec.Permissions.Env))
	for _, entry := range spec.Permissions.Env {
		switch {
		case entry.Value != "":
			resolved[entry.Name] = entry.Value
		case entry.Secret != "":
			val, err := rt.secrets.Resolve(ctx, entry.Secret)
			if err != nil {
				status = registry.StatusFailure
				result.Error = fmt.Errorf("resolve secret %q for env %q: %w", entry.Secret, entry.Name, err)
				return result, nil
			}
			resolved[entry.Name] = val
		case entry.From != "":
			resolved[entry.Name] = os.Getenv(entry.From)
			// bare name: --allow-env only, no injection
		}
	}

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

	srv := ipc.New(runID, spec.ID, rt.secret, rt.registry, rt.db, mergedParams, opts.Input, rt.log, spec, rt.engine, rt.aiBaseURL, rt.aiModel, rt.aiAPIKey)
	srv.SetGateway(rt.gateway)
	srv.SetSecrets(rt.secretsManager)
	socketPath, token, err := srv.Start(execCtx)
	if err != nil {
		status = registry.StatusFailure
		result.Error = fmt.Errorf("start socket server: %w", err)
		return result, nil
	}
	defer srv.Stop()

	debug := rt.log.Core().Enabled(zap.DebugLevel)

	// Write the shim as a proper ES module to a temp file.
	shimFile, err := os.CreateTemp("", "dicode-shim-*.ts")
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
	runnerFile, err := os.CreateTemp("", "dicode-runner-*.ts")
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

	args := buildDenoArgs(spec, socketPath, shimPath, runnerPath)
	cmd := exec.CommandContext(execCtx, rt.denoPath, args...) //nolint:gosec
	cmd.Env = buildEnv(resolved, socketPath, token)

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

	// Stream stdout (console.log/info) as "info" and stderr (console.error +
	// Deno runtime errors) as "error" in the run log.
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			_ = rt.registry.AppendLog(context.Background(), runID, "info", scanner.Text())
		}
	}()
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			_ = rt.registry.AppendLog(context.Background(), runID, "error", scanner.Text())
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

func buildDenoArgs(spec *task.Spec, socketPath, shimPath, runnerPath string) []string {
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
	envVars := []string{"DICODE_SOCKET", "DICODE_TOKEN", "HOME", "DENO_DIR", "XDG_CACHE_HOME"}
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

func buildEnv(resolved map[string]string, socketPath, token string) []string {
	// Inherit the host environment so Deno can locate its cache (DENO_DIR etc).
	// The --allow-env flag separately controls which vars the JS script can read.
	env := append(os.Environ(), "DICODE_SOCKET="+socketPath, "DICODE_TOKEN="+token)
	for k, v := range resolved {
		env = append(env, k+"="+v)
	}
	return env
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
