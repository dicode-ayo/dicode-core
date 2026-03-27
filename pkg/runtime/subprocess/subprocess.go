// Package subprocess executes task scripts by spawning an interpreter subprocess.
// It supports any language where a task can be run as:
//
//	<interpreter> [extra-args...] <script-file>
//
// Stdout is logged at info level, stderr at warn level.
// The script file must be named task<ScriptExt> inside the task directory.
//
// # Run context available to scripts
//
// Every subprocess run receives the following environment variables in addition
// to the normal host environment:
//
//	DICODE_RUN_ID            — the current run ID
//	DICODE_PARAM_<NAME>      — value of each task parameter (name uppercased)
//
// Param defaults from task.yaml are merged with any per-run overrides before
// the environment is built.
package subprocess

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// Executor runs task scripts via a subprocess interpreter.
type Executor struct {
	// Interpreter is the binary to invoke (e.g. "python3", "julia", "ruby").
	Interpreter string
	// Args are extra arguments inserted between the interpreter and the script path.
	Args []string
	// ScriptExt is the file extension for task scripts (e.g. ".py", ".jl", ".rb").
	ScriptExt string

	registry *registry.Registry
	log      *zap.Logger
}

// New creates a subprocess Executor.
func New(interpreter string, args []string, scriptExt string, reg *registry.Registry, log *zap.Logger) *Executor {
	return &Executor{
		Interpreter: interpreter,
		Args:        args,
		ScriptExt:   scriptExt,
		registry:    reg,
		log:         log,
	}
}

// Execute implements runtime.Executor.
func (e *Executor) Execute(ctx context.Context, spec *task.Spec, opts pkgruntime.RunOptions) (*pkgruntime.RunResult, error) {
	runID := opts.RunID
	result := &pkgruntime.RunResult{RunID: runID}

	scriptPath := filepath.Join(spec.TaskDir, "task"+e.ScriptExt)
	if _, err := os.Stat(scriptPath); err != nil {
		result.Error = fmt.Errorf("script not found: %s", scriptPath)
		_ = e.registry.FinishRun(context.Background(), runID, registry.StatusFailure)
		return result, nil
	}

	args := make([]string, 0, len(e.Args)+1)
	args = append(args, e.Args...)
	args = append(args, scriptPath)

	cmd := exec.CommandContext(ctx, e.Interpreter, args...) //nolint:gosec
	cmd.Env = buildEnv(spec, opts)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		result.Error = err
		_ = e.registry.FinishRun(context.Background(), runID, registry.StatusFailure)
		return result, nil
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		result.Error = err
		_ = e.registry.FinishRun(context.Background(), runID, registry.StatusFailure)
		return result, nil
	}

	if err := cmd.Start(); err != nil {
		result.Error = fmt.Errorf("start %s: %w", e.Interpreter, err)
		_ = e.registry.FinishRun(context.Background(), runID, registry.StatusFailure)
		return result, nil
	}

	e.log.Info("subprocess started",
		zap.String("task", spec.ID),
		zap.String("run", runID),
		zap.String("interpreter", e.Interpreter),
	)

	// Stream stdout and stderr to the run log concurrently.
	stdoutDone := make(chan struct{})
	stderrDone := make(chan struct{})
	go func() {
		defer close(stdoutDone)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			_ = e.registry.AppendLog(context.Background(), runID, "info", scanner.Text())
		}
	}()
	go func() {
		defer close(stderrDone)
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			_ = e.registry.AppendLog(context.Background(), runID, "warn", scanner.Text())
		}
	}()

	exitErr := cmd.Wait()
	<-stdoutDone
	<-stderrDone

	var finalStatus string
	switch {
	case ctx.Err() != nil:
		finalStatus = registry.StatusCancelled
	case exitErr != nil:
		finalStatus = registry.StatusFailure
		result.Error = exitErr
	default:
		finalStatus = registry.StatusSuccess
	}

	e.log.Info("subprocess finished",
		zap.String("task", spec.ID),
		zap.String("run", runID),
		zap.String("status", finalStatus),
	)
	_ = e.registry.FinishRun(context.Background(), runID, finalStatus)
	return result, nil
}

// buildEnv constructs the subprocess environment.
// It starts from the host environment and adds DICODE_RUN_ID plus
// DICODE_PARAM_<NAME> for every task parameter (defaults merged with overrides).
func buildEnv(spec *task.Spec, opts pkgruntime.RunOptions) []string {
	// Merge spec param defaults with per-run overrides.
	merged := make(map[string]string, len(spec.Params))
	for _, p := range spec.Params {
		merged[p.Name] = p.Default
	}
	for k, v := range opts.Params {
		merged[k] = v
	}

	extra := make([]string, 0, 1+len(merged))
	extra = append(extra, "DICODE_RUN_ID="+opts.RunID)
	for k, v := range merged {
		extra = append(extra, "DICODE_PARAM_"+strings.ToUpper(k)+"="+v)
	}
	return append(os.Environ(), extra...)
}
