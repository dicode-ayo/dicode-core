// Package js executes task scripts using the goja JavaScript engine.
// Each call to Run creates a fresh, isolated goja runtime + event loop.
package js

import (
	"context"
	"fmt"
	"time"

	"github.com/dop251/goja"
	"github.com/dop251/goja_nodejs/eventloop"
	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/runtime/js/globals"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// RunOptions controls a single task execution.
type RunOptions struct {
	RunID           string // if non-empty, use this run ID instead of generating one
	Params          map[string]string
	Input           interface{}
	ParentRunID     string
	HTTPInterceptor globals.HTTPInterceptor
}

// RunResult is returned by Run.
type RunResult struct {
	RunID       string
	ReturnValue interface{}
	Output      *globals.OutputResult
	Logs        []*registry.LogEntry
	Error       error
}

// Runtime executes JS task scripts with injected globals.
type Runtime struct {
	registry *registry.Registry
	secrets  secrets.Chain
	db       db.DB
	log      *zap.Logger
}

// New creates a JS Runtime.
func New(r *registry.Registry, sc secrets.Chain, database db.DB, log *zap.Logger) *Runtime {
	return &Runtime{registry: r, secrets: sc, db: database, log: log}
}

// Run executes a task script and returns the result.
func (rt *Runtime) Run(ctx context.Context, spec *task.Spec, opts RunOptions) (*RunResult, error) {
	if opts.Params == nil {
		opts.Params = map[string]string{}
	}

	var runID string
	var err error
	if opts.RunID != "" {
		// Run record already created by the engine (async path).
		runID = opts.RunID
	} else {
		runID, err = rt.registry.StartRun(ctx, spec.ID, opts.ParentRunID)
		if err != nil {
			return nil, fmt.Errorf("start run: %w", err)
		}
	}

	sink := newLogSink(runID, rt.registry, ctx, rt.log)
	result := &RunResult{RunID: runID}
	status := registry.StatusSuccess

	defer func() {
		result.Logs = sink.entries
		// Use context.Background() so FinishRun succeeds even if ctx was cancelled.
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

	execResult, execErr := execute(execCtx, spec, script, resolved, sink, rt.db, rt.log, opts)
	if execErr != nil {
		if ctx.Err() != nil {
			status = registry.StatusCancelled
		} else {
			status = registry.StatusFailure
		}
		result.Error = execErr
		return result, nil
	}

	result.Output = execResult.output
	result.ReturnValue = execResult.returnValue
	return result, nil
}

type execResult struct {
	returnValue interface{}
	output      *globals.OutputResult
}

// execute runs the script inside a goja event loop.
// The loop blocks until all async operations settle or ctx is cancelled.
func execute(
	ctx context.Context,
	spec *task.Spec,
	script string,
	resolved map[string]string,
	sink *logSink,
	database db.DB,
	log *zap.Logger,
	opts RunOptions,
) (*execResult, error) {
	loop := eventloop.NewEventLoop()
	result := &execResult{}
	var runErr error

	// context cancellation: terminate the loop
	go func() {
		<-ctx.Done()
		loop.Terminate()
	}()

	loop.Run(func(vm *goja.Runtime) {
		globals.InjectLog(vm, sink, log)
		globals.InjectEnv(vm, resolved)
		globals.InjectParams(vm, spec, opts.Params)
		globals.InjectHTTP(vm, opts.HTTPInterceptor)
		globals.InjectKV(vm, database, spec.ID)
		outputResult := globals.InjectOutput(vm)

		if len(spec.FS) > 0 {
			globals.InjectFS(vm, spec.FS)
		}
		if opts.Input != nil {
			vm.Set("input", vm.ToValue(opts.Input))
		}

		// Capture return value / error via JS callbacks.
		vm.Set("__resolve__", func(v goja.Value) {
			if outputResult.IsSet() {
				result.output = outputResult
			} else if v != nil && !goja.IsUndefined(v) && !goja.IsNull(v) {
				result.returnValue = v.Export()
			}
		})
		vm.Set("__reject__", func(v goja.Value) {
			msg := "task error"
			if v != nil {
				msg = v.String()
			}
			runErr = fmt.Errorf("%s", msg)
		})

		wrapped := fmt.Sprintf(`(async function() { %s })().then(__resolve__).catch(__reject__)`, script)
		if _, err := vm.RunString(wrapped); err != nil {
			runErr = fmt.Errorf("script error: %w", err)
		}
	})

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return result, runErr
}

// logSink captures log calls and persists them to sqlite.
type logSink struct {
	runID    string
	registry *registry.Registry
	ctx      context.Context
	entries  []*registry.LogEntry
	zapLog   *zap.Logger
}

func newLogSink(runID string, r *registry.Registry, ctx context.Context, log *zap.Logger) *logSink {
	return &logSink{runID: runID, registry: r, ctx: ctx, zapLog: log}
}

func (s *logSink) AppendLog(level, msg string) {
	entry := &registry.LogEntry{
		RunID:   s.runID,
		Ts:      time.Now(),
		Level:   level,
		Message: msg,
	}
	s.entries = append(s.entries, entry)
	_ = s.registry.AppendLog(s.ctx, s.runID, level, msg)
}
