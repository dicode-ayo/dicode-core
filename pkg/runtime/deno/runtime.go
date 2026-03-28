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
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	denoserver "github.com/dicode/dicode/pkg/runtime/deno/server"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

//go:embed sdk/shim.js
var shimContent string

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
	Output      *denoserver.OutputResult
	Logs        []*registry.LogEntry
	Error       error
}

// Runtime executes task scripts with Deno.
type Runtime struct {
	registry *registry.Registry
	secrets  secrets.Chain
	db       db.DB
	log      *zap.Logger
	denoPath string
}

// New creates a Deno Runtime. It ensures the Deno binary is present in the
// cache, downloading it if necessary.
func New(r *registry.Registry, sc secrets.Chain, database db.DB, log *zap.Logger) (*Runtime, error) {
	path, err := denopkg.EnsureDeno(denopkg.DefaultVersion)
	if err != nil {
		return nil, fmt.Errorf("ensure deno: %w", err)
	}
	return &Runtime{registry: r, secrets: sc, db: database, log: log, denoPath: path}, nil
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

	resolved, err := rt.secrets.ResolveAll(ctx, spec.Env)
	if err != nil {
		status = registry.StatusFailure
		result.Error = err
		return result, nil
	}

	script, err := spec.Script()
	if err != nil {
		status = registry.StatusFailure
		result.Error = fmt.Errorf("read script: %w", err)
		return result, nil
	}

	timeout := spec.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}

	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	mergedParams := mergeParams(spec.Params, opts.Params)

	srv := denoserver.New(runID, spec.ID, rt.registry, rt.db, mergedParams, opts.Input, rt.log)
	socketPath, err := srv.Start(execCtx)
	if err != nil {
		status = registry.StatusFailure
		result.Error = fmt.Errorf("start socket server: %w", err)
		return result, nil
	}
	defer srv.Stop()

	tmpFile, err := os.CreateTemp("", "dicode-task-*.ts")
	if err != nil {
		status = registry.StatusFailure
		result.Error = fmt.Errorf("create temp file: %w", err)
		return result, nil
	}
	defer os.Remove(tmpFile.Name())

	// Wrap the user script in try/finally so the connection is always closed,
	// allowing the shim's background reader loop to exit and Deno to terminate
	// cleanly even when the script throws an unhandled exception.
	wrapped := shimContent + "\n\ntry {\nconst __result__ = await (async () => {\n" + script + "\n})();\nawait __setReturn__(__result__);\n} finally {\ntry { __conn__.close(); } catch {}\n}\n"
	if _, err := tmpFile.WriteString(wrapped); err != nil {
		tmpFile.Close()
		status = registry.StatusFailure
		result.Error = fmt.Errorf("write script: %w", err)
		return result, nil
	}
	tmpFile.Close()

	args := buildDenoArgs(spec, socketPath, tmpFile.Name())
	cmd := exec.CommandContext(execCtx, rt.denoPath, args...) //nolint:gosec
	cmd.Env = buildEnv(resolved, socketPath)

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

	// Stream deno stderr to registry logs in real-time.
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			_ = rt.registry.AppendLog(context.Background(), runID, "warn", scanner.Text())
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
		out[p.Name] = p.Default
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

func buildDenoArgs(spec *task.Spec, socketPath, scriptPath string) []string {
	args := []string{"run", "--allow-net"}

	envVars := append([]string{"DICODE_SOCKET"}, spec.Env...)
	args = append(args, "--allow-env="+strings.Join(envVars, ","))

	// Deno 2.x requires explicit read+write permission for Unix socket paths.
	readPaths := []string{socketPath}
	writePaths := []string{socketPath}
	for _, entry := range spec.FS {
		switch entry.Permission {
		case "r":
			readPaths = append(readPaths, entry.Path)
		case "w":
			writePaths = append(writePaths, entry.Path)
		case "rw":
			readPaths = append(readPaths, entry.Path)
			writePaths = append(writePaths, entry.Path)
		}
	}
	args = append(args, "--allow-read="+strings.Join(readPaths, ","))
	args = append(args, "--allow-write="+strings.Join(writePaths, ","))

	args = append(args, scriptPath)
	return args
}

func buildEnv(resolved map[string]string, socketPath string) []string {
	// Inherit the host environment so Deno can locate its cache (DENO_DIR etc).
	// The --allow-env flag separately controls which vars the JS script can read.
	env := append(os.Environ(), "DICODE_SOCKET="+socketPath)
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
