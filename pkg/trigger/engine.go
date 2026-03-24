// Package trigger manages cron schedules, webhook dispatch, manual fires,
// and chain reactions between tasks.
package trigger

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/dicode/dicode/pkg/registry"
	dockerruntime "github.com/dicode/dicode/pkg/runtime/docker"
	jsruntime "github.com/dicode/dicode/pkg/runtime/js"
	"github.com/dicode/dicode/pkg/task"
	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
)

// Engine coordinates all trigger types and fires task runs.
type Engine struct {
	registry *registry.Registry
	jsRT     *jsruntime.Runtime
	dockerRT *dockerruntime.Runtime
	cron     *cron.Cron
	log      *zap.Logger

	mu          sync.Mutex
	cronEntries map[string]cron.EntryID // taskID → cron entry
	webhooks    map[string]string        // webhook path → taskID

	runCancels sync.Map // runID → context.CancelFunc
}

// New creates a trigger Engine.
func New(r *registry.Registry, rt *jsruntime.Runtime, log *zap.Logger) *Engine {
	return &Engine{
		registry:    r,
		jsRT:        rt,
		cron:        cron.New(),
		log:         log,
		cronEntries: make(map[string]cron.EntryID),
		webhooks:    make(map[string]string),
	}
}

// SetDockerRuntime wires the Docker executor into the engine.
func (e *Engine) SetDockerRuntime(rt *dockerruntime.Runtime) {
	e.dockerRT = rt
}

// Start begins cron scheduling and runs until ctx is cancelled.
func (e *Engine) Start(ctx context.Context) error {
	// Register all tasks already in the registry.
	for _, spec := range e.registry.All() {
		e.Register(spec)
	}
	e.cron.Start()
	<-ctx.Done()
	e.cron.Stop()
	return nil
}

// Register adds or updates trigger registrations for a task spec.
// Called by the reconciler's OnRegister callback.
func (e *Engine) Register(spec *task.Spec) {
	e.Unregister(spec.ID) // remove old registrations first

	if spec.Trigger.Cron != "" {
		e.registerCron(spec)
	}
	if spec.Trigger.Webhook != "" {
		e.registerWebhook(spec)
	}
}

// Unregister removes all trigger registrations for a task ID.
func (e *Engine) Unregister(id string) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if entryID, ok := e.cronEntries[id]; ok {
		e.cron.Remove(entryID)
		delete(e.cronEntries, id)
	}
	for path, tid := range e.webhooks {
		if tid == id {
			delete(e.webhooks, path)
		}
	}
}

func (e *Engine) registerCron(spec *task.Spec) {
	id, err := e.cron.AddFunc(spec.Trigger.Cron, func() {
		s, ok := e.registry.Get(spec.ID)
		if !ok {
			return
		}
		e.fireAsync(context.Background(), s, jsruntime.RunOptions{})
	})
	if err != nil {
		e.log.Error("invalid cron expression",
			zap.String("task", spec.ID),
			zap.String("cron", spec.Trigger.Cron),
			zap.Error(err),
		)
		return
	}
	e.mu.Lock()
	e.cronEntries[spec.ID] = id
	e.mu.Unlock()
	e.log.Info("cron registered", zap.String("task", spec.ID), zap.String("expr", spec.Trigger.Cron))
}

func (e *Engine) registerWebhook(spec *task.Spec) {
	e.mu.Lock()
	e.webhooks[spec.Trigger.Webhook] = spec.ID
	e.mu.Unlock()
	e.log.Info("webhook registered", zap.String("task", spec.ID), zap.String("path", spec.Trigger.Webhook))
}

// FireManual triggers a task by ID with optional param overrides.
// Returns the run ID immediately (fire is asynchronous).
func (e *Engine) FireManual(ctx context.Context, taskID string, params map[string]string) (string, error) {
	spec, ok := e.registry.Get(taskID)
	if !ok {
		return "", fmt.Errorf("task %q not found", taskID)
	}
	return e.fireAsync(context.Background(), spec, jsruntime.RunOptions{Params: params})
}

// KillRun cancels a running task by its run ID.
// Returns true if the run was found and cancelled, false if not found.
func (e *Engine) KillRun(runID string) bool {
	v, ok := e.runCancels.Load(runID)
	if !ok {
		return false
	}
	v.(context.CancelFunc)()
	return true
}

// FireChain checks if any tasks declare a chain trigger from completedTaskID
// and fires them with the given output as input.
func (e *Engine) FireChain(ctx context.Context, completedTaskID, runStatus string, output interface{}) {
	for _, spec := range e.registry.All() {
		chain := spec.Trigger.Chain
		if chain == nil || chain.From != completedTaskID {
			continue
		}
		on := chain.ChainOn()
		if on != "always" && on != runStatus {
			continue
		}
		go e.fireAsync(ctx, spec, jsruntime.RunOptions{Input: output}) //nolint:errcheck
	}
}

// WebhookHandler returns an HTTP handler that dispatches webhook-triggered tasks.
func (e *Engine) WebhookHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		e.mu.Lock()
		taskID, ok := e.webhooks[path]
		e.mu.Unlock()

		if !ok {
			http.NotFound(w, r)
			return
		}

		spec, ok := e.registry.Get(taskID)
		if !ok {
			http.Error(w, "task not found", http.StatusNotFound)
			return
		}

		// Parse body as JSON input.
		var input interface{}
		if r.Body != nil {
			body, err := io.ReadAll(r.Body)
			if err == nil && len(body) > 0 {
				_ = json.Unmarshal(body, &input)
			}
		}

		runID, err := e.fireAsync(r.Context(), spec, jsruntime.RunOptions{Input: input})
		if err != nil {
			http.Error(w, "task failed to start", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"runId": runID})
	})
}

// fireAsync pre-creates the run record, starts execution in a goroutine,
// and returns the run ID immediately. This allows callers to poll for status.
func (e *Engine) fireAsync(ctx context.Context, spec *task.Spec, opts jsruntime.RunOptions) (string, error) {
	runID := uuid.New().String()
	opts.RunID = runID

	if _, err := e.registry.StartRunWithID(context.Background(), runID, spec.ID, opts.ParentRunID); err != nil {
		return "", fmt.Errorf("start run record: %w", err)
	}

	runCtx, runCancel := context.WithCancel(context.Background())
	e.runCancels.Store(runID, runCancel)

	go func() {
		defer func() {
			e.runCancels.Delete(runID)
			runCancel()
		}()
		e.log.Info("firing task", zap.String("task", spec.ID), zap.String("run", runID))
		e.dispatch(runCtx, spec, opts)
	}()

	return runID, nil
}

// dispatch routes a run to the appropriate runtime based on spec.Runtime.
func (e *Engine) dispatch(ctx context.Context, spec *task.Spec, opts jsruntime.RunOptions) {
	switch spec.Runtime {
	case task.RuntimeDocker:
		if e.dockerRT == nil {
			e.log.Error("docker runtime not configured", zap.String("task", spec.ID))
			_ = e.registry.FinishRun(context.Background(), opts.RunID, registry.StatusFailure)
			return
		}
		dockerOpts := dockerruntime.RunOptions{
			RunID:       opts.RunID,
			ParentRunID: opts.ParentRunID,
			Params:      opts.Params,
		}
		result, err := e.dockerRT.Run(ctx, spec, dockerOpts)
		if err != nil {
			e.log.Error("docker run error", zap.String("task", spec.ID), zap.Error(err))
			return
		}
		status := "success"
		if result.Error != nil {
			if ctx.Err() != nil {
				status = registry.StatusCancelled
			} else {
				status = registry.StatusFailure
			}
			e.log.Warn("docker task ended", zap.String("task", spec.ID), zap.String("status", status), zap.Error(result.Error))
		}
		e.FireChain(context.Background(), spec.ID, status, nil)

	default: // RuntimeJS
		result, err := e.jsRT.Run(ctx, spec, opts)
		if err != nil {
			e.log.Error("js run error", zap.String("task", spec.ID), zap.Error(err))
			return
		}
		status := "success"
		if result.Error != nil {
			if ctx.Err() != nil {
				status = registry.StatusCancelled
			} else {
				status = registry.StatusFailure
			}
			e.log.Warn("task failed", zap.String("task", spec.ID), zap.Error(result.Error))
		}
		var chainInput interface{}
		if result.Output != nil {
			chainInput = result.Output.Data
		} else {
			chainInput = result.ReturnValue
		}
		e.FireChain(context.Background(), spec.ID, status, chainInput)
	}
}
