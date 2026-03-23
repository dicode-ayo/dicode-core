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
	jsruntime "github.com/dicode/dicode/pkg/runtime/js"
	"github.com/dicode/dicode/pkg/task"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
)

// Engine coordinates all trigger types and fires task runs.
type Engine struct {
	registry *registry.Registry
	runtime  *jsruntime.Runtime
	cron     *cron.Cron
	log      *zap.Logger

	mu          sync.Mutex
	cronEntries map[string]cron.EntryID // taskID → cron entry
	webhooks    map[string]string        // webhook path → taskID
}

// New creates a trigger Engine.
func New(r *registry.Registry, rt *jsruntime.Runtime, log *zap.Logger) *Engine {
	return &Engine{
		registry:    r,
		runtime:     rt,
		cron:        cron.New(),
		log:         log,
		cronEntries: make(map[string]cron.EntryID),
		webhooks:    make(map[string]string),
	}
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
		e.fire(context.Background(), s, jsruntime.RunOptions{})
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
// Returns the run ID.
func (e *Engine) FireManual(ctx context.Context, taskID string, params map[string]string) (string, error) {
	spec, ok := e.registry.Get(taskID)
	if !ok {
		return "", fmt.Errorf("task %q not found", taskID)
	}
	result := e.fire(ctx, spec, jsruntime.RunOptions{Params: params})
	if result == nil {
		return "", fmt.Errorf("run failed to start")
	}
	return result.RunID, result.Error
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
		go e.fire(ctx, spec, jsruntime.RunOptions{Input: output})
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

		result := e.fire(r.Context(), spec, jsruntime.RunOptions{Input: input})
		if result == nil || result.Error != nil {
			http.Error(w, "task failed", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"runId": result.RunID})
	})
}

// fire executes a task and triggers any chain reactions on completion.
func (e *Engine) fire(ctx context.Context, spec *task.Spec, opts jsruntime.RunOptions) *jsruntime.RunResult {
	e.log.Info("firing task", zap.String("task", spec.ID))
	result, err := e.runtime.Run(ctx, spec, opts)
	if err != nil {
		e.log.Error("run error", zap.String("task", spec.ID), zap.Error(err))
		return nil
	}
	if result.Error != nil {
		e.log.Warn("task failed", zap.String("task", spec.ID), zap.Error(result.Error))
	}

	// Trigger chain reactions.
	status := "success"
	if result.Error != nil {
		status = "failure"
	}
	var chainInput interface{}
	if result.Output != nil {
		chainInput = result.Output.Data
	} else {
		chainInput = result.ReturnValue
	}
	e.FireChain(ctx, spec.ID, status, chainInput)

	return result
}
