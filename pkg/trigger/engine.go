// Package trigger manages cron schedules, webhook dispatch, manual fires,
// chain reactions, and daemon (always-on) tasks.
package trigger

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

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

	shutdownMu  sync.RWMutex
	shutdownCtx context.Context

	daemonMu    sync.Mutex
	daemonRuns  map[string]string
	daemonSpecs map[string]*task.Spec
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
		daemonRuns:  make(map[string]string),
		daemonSpecs: make(map[string]*task.Spec),
	}
}

// SetDockerRuntime wires the Docker executor into the engine.
func (e *Engine) SetDockerRuntime(rt *dockerruntime.Runtime) {
	e.dockerRT = rt
}

// Start begins scheduling and runs until ctx is cancelled.
func (e *Engine) Start(ctx context.Context) error {
	e.shutdownMu.Lock()
	e.shutdownCtx = ctx
	e.shutdownMu.Unlock()

	for _, spec := range e.registry.All() {
		e.Register(spec)
	}
	e.cron.Start()

	<-ctx.Done()
	e.cron.Stop()

	e.daemonMu.Lock()
	killList := make(map[string]string, len(e.daemonRuns))
	for k, v := range e.daemonRuns {
		killList[k] = v
	}
	e.daemonMu.Unlock()

	for taskID, runID := range killList {
		e.log.Info("stopping daemon on shutdown", zap.String("task", taskID), zap.String("run", runID))
		e.KillRun(runID)
	}
	return nil
}

// Register adds or updates trigger registrations for a task spec.
func (e *Engine) Register(spec *task.Spec) {
	e.Unregister(spec.ID)

	if spec.Trigger.Cron != "" {
		e.registerCron(spec)
	}
	if spec.Trigger.Webhook != "" {
		e.registerWebhook(spec)
	}
	if spec.Trigger.Daemon {
		e.registerDaemon(spec)
	}
	e.log.Info("task registered",
		zap.String("task", spec.ID),
		zap.String("trigger", triggerSource(spec)),
		zap.String("runtime", string(spec.Runtime)),
	)
}

// Unregister removes all trigger registrations for a task ID.
func (e *Engine) Unregister(id string) {
	e.mu.Lock()
	if entryID, ok := e.cronEntries[id]; ok {
		e.cron.Remove(entryID)
		delete(e.cronEntries, id)
	}
	for path, tid := range e.webhooks {
		if tid == id {
			delete(e.webhooks, path)
		}
	}
	e.mu.Unlock()

	e.daemonMu.Lock()
	delete(e.daemonSpecs, id)
	runID := e.daemonRuns[id]
	delete(e.daemonRuns, id)
	e.daemonMu.Unlock()

	if runID != "" {
		e.log.Info("stopping daemon — task unregistered", zap.String("task", id), zap.String("run", runID))
		e.KillRun(runID)
	}
	e.log.Info("task unregistered", zap.String("task", id))
}

func (e *Engine) registerCron(spec *task.Spec) {
	id, err := e.cron.AddFunc(spec.Trigger.Cron, func() {
		s, ok := e.registry.Get(spec.ID)
		if !ok {
			return
		}
		e.fireAsync(context.Background(), s, jsruntime.RunOptions{}, "cron") //nolint:errcheck
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
}

func (e *Engine) registerWebhook(spec *task.Spec) {
	e.mu.Lock()
	e.webhooks[spec.Trigger.Webhook] = spec.ID
	e.mu.Unlock()
}

func (e *Engine) registerDaemon(spec *task.Spec) {
	e.daemonMu.Lock()
	e.daemonSpecs[spec.ID] = spec
	_, alreadyRunning := e.daemonRuns[spec.ID]
	e.daemonMu.Unlock()

	if alreadyRunning {
		return
	}
	e.startDaemon(spec)
}

func (e *Engine) startDaemon(spec *task.Spec) {
	runID, err := e.fireAsync(context.Background(), spec, jsruntime.RunOptions{}, "daemon")
	if err != nil {
		e.log.Error("daemon start failed", zap.String("task", spec.ID), zap.Error(err))
		return
	}
	e.daemonMu.Lock()
	e.daemonRuns[spec.ID] = runID
	e.daemonMu.Unlock()
}

func (e *Engine) onDaemonRunFinished(spec *task.Spec, runID string) {
	e.daemonMu.Lock()
	if e.daemonRuns[spec.ID] == runID {
		delete(e.daemonRuns, spec.ID)
	}
	_, stillRegistered := e.daemonSpecs[spec.ID]
	e.daemonMu.Unlock()

	if !stillRegistered || e.isShuttingDown() {
		return
	}

	run, err := e.registry.GetRun(context.Background(), runID)
	if err != nil {
		e.log.Error("daemon: failed to get run status", zap.String("run", runID), zap.Error(err))
		return
	}
	if run.Status == registry.StatusCancelled {
		return
	}

	restart := spec.Trigger.Restart
	if restart == "" {
		restart = "always"
	}
	switch restart {
	case "never":
		e.log.Info("daemon exited — restart=never, not restarting",
			zap.String("task", spec.ID), zap.String("status", run.Status))
		return
	case "on-failure":
		if run.Status != registry.StatusFailure {
			e.log.Info("daemon exited — restart=on-failure, not restarting (no failure)",
				zap.String("task", spec.ID), zap.String("status", run.Status))
			return
		}
	}

	e.log.Info("daemon exited, scheduling restart",
		zap.String("task", spec.ID),
		zap.String("status", run.Status),
		zap.String("restart", restart),
	)

	shutCtx := e.getShutdownCtx()
	if shutCtx == nil {
		shutCtx = context.Background()
	}
	select {
	case <-shutCtx.Done():
		return
	case <-time.After(2 * time.Second):
	}

	if !e.isShuttingDown() {
		e.log.Info("restarting daemon task", zap.String("task", spec.ID))
		e.startDaemon(spec)
	}
}

func (e *Engine) isShuttingDown() bool {
	e.shutdownMu.RLock()
	ctx := e.shutdownCtx
	e.shutdownMu.RUnlock()
	return ctx != nil && ctx.Err() != nil
}

func (e *Engine) getShutdownCtx() context.Context {
	e.shutdownMu.RLock()
	defer e.shutdownMu.RUnlock()
	return e.shutdownCtx
}

// FireManual triggers a task by ID with optional param overrides.
func (e *Engine) FireManual(ctx context.Context, taskID string, params map[string]string) (string, error) {
	spec, ok := e.registry.Get(taskID)
	if !ok {
		return "", fmt.Errorf("task %q not found", taskID)
	}
	e.log.Info("manual trigger", zap.String("task", taskID))
	return e.fireAsync(context.Background(), spec, jsruntime.RunOptions{Params: params}, "manual")
}

// KillRun cancels a running task by its run ID.
func (e *Engine) KillRun(runID string) bool {
	v, ok := e.runCancels.Load(runID)
	if !ok {
		return false
	}
	e.log.Info("run kill requested", zap.String("run", runID))
	v.(context.CancelFunc)()
	return true
}

// FireChain checks if any tasks declare a chain trigger from completedTaskID.
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
		e.log.Info("chain trigger",
			zap.String("from", completedTaskID),
			zap.String("to", spec.ID),
			zap.String("on", on),
		)
		go e.fireAsync(ctx, spec, jsruntime.RunOptions{Input: output}, "chain") //nolint:errcheck
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

		e.log.Info("webhook trigger", zap.String("path", path), zap.String("task", taskID))

		var input interface{}
		if r.Body != nil {
			body, err := io.ReadAll(r.Body)
			if err == nil && len(body) > 0 {
				_ = json.Unmarshal(body, &input)
			}
		}

		runID, err := e.fireAsync(r.Context(), spec, jsruntime.RunOptions{Input: input}, "webhook")
		if err != nil {
			http.Error(w, "task failed to start", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"runId": runID})
	})
}

// fireAsync pre-creates the run record, starts execution in a goroutine,
// and returns the run ID immediately.
// source identifies how the run was triggered ("manual","cron","webhook","chain","daemon").
func (e *Engine) fireAsync(ctx context.Context, spec *task.Spec, opts jsruntime.RunOptions, source string) (string, error) {
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

		e.log.Info("run started",
			zap.String("task", spec.ID),
			zap.String("run", runID),
			zap.String("trigger", source),
			zap.String("runtime", string(spec.Runtime)),
		)

		start := time.Now()
		status := e.dispatch(runCtx, spec, opts)
		elapsed := time.Since(start)

		e.log.Info("run finished",
			zap.String("task", spec.ID),
			zap.String("run", runID),
			zap.String("status", status),
			zap.String("trigger", source),
			zap.Duration("duration", elapsed.Truncate(time.Millisecond)),
		)

		if spec.Trigger.Daemon {
			e.onDaemonRunFinished(spec, runID)
		}
	}()

	return runID, nil
}

// dispatch routes a run to the appropriate runtime and returns the final status.
func (e *Engine) dispatch(ctx context.Context, spec *task.Spec, opts jsruntime.RunOptions) string {
	switch spec.Runtime {
	case task.RuntimeDocker:
		if e.dockerRT == nil {
			e.log.Error("docker runtime not configured", zap.String("task", spec.ID))
			_ = e.registry.FinishRun(context.Background(), opts.RunID, registry.StatusFailure)
			return registry.StatusFailure
		}
		dockerOpts := dockerruntime.RunOptions{
			RunID:       opts.RunID,
			ParentRunID: opts.ParentRunID,
			Params:      opts.Params,
		}
		result, err := e.dockerRT.Run(ctx, spec, dockerOpts)
		if err != nil {
			e.log.Error("docker run error", zap.String("task", spec.ID), zap.Error(err))
			return registry.StatusFailure
		}
		status := registry.StatusSuccess
		if result.Error != nil {
			if ctx.Err() != nil {
				status = registry.StatusCancelled
			} else {
				status = registry.StatusFailure
			}
		}
		e.FireChain(context.Background(), spec.ID, status, nil)
		return status

	default: // RuntimeJS
		result, err := e.jsRT.Run(ctx, spec, opts)
		if err != nil {
			e.log.Error("js run error", zap.String("task", spec.ID), zap.Error(err))
			return registry.StatusFailure
		}
		status := registry.StatusSuccess
		if result.Error != nil {
			if ctx.Err() != nil {
				status = registry.StatusCancelled
			} else {
				status = registry.StatusFailure
			}
		}
		var chainInput interface{}
		if result.Output != nil {
			chainInput = result.Output.Data
		} else {
			chainInput = result.ReturnValue
		}
		e.FireChain(context.Background(), spec.ID, status, chainInput)
		return status
	}
}

// triggerSource returns a short string identifying the trigger type of a spec.
func triggerSource(spec *task.Spec) string {
	switch {
	case spec.Trigger.Cron != "":
		return "cron"
	case spec.Trigger.Webhook != "":
		return "webhook"
	case spec.Trigger.Daemon:
		return "daemon"
	case spec.Trigger.Chain != nil:
		return "chain"
	default:
		return "manual"
	}
}
