// This file owns the Run lifecycle: the span from the decision to fire a task
// of any kind through to the run row reaching a terminal state and every
// registration keyed on its run ID being released.

package trigger

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dicode/dicode/pkg/audit"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// runHandle is one Run's bookkeeping, held by whoever is executing it.
//
// Contract:
//
//   - beginRun can be refused. A fire-guard veto or a failed run-row insert
//     returns an error and no handle; the caller must not proceed.
//   - finish is exactly-once, and so is the teardown backstop. Whichever runs
//     first wins; the other is a no-op, so a caller can defer teardown and
//     still call finish on the normal path.
//   - The run row outlives the handle. finish releases in-memory
//     registrations only; the caller has already made the row terminal.
//   - The run:finished hooks and downstream chain edges fire from finish, so
//     every kind observes them.
type runHandle struct {
	e      *Engine
	taskID string
	kind   string
	runID  string
	source registry.TriggerSource

	// chainParentTask names the upstream whose on_failure_chain concurrency
	// slot this run occupies, or "" when the run holds no slot.
	chainParentTask string

	cancel    context.CancelFunc
	startedAt time.Time
	once      sync.Once
}

// runFinish is the outcome a caller hands to runHandle.finish. dispatch builds
// most of it; the caller adds the fields dispatch does not know.
//
// chain is carried rather than derived from status: a run that never reached
// its executor (no executor for the runtime, an unsatisfied if_missing prereq,
// a rejected hand-off) promises downstream subscribers nothing even though it
// failed, and a suspended run promises nothing because it is not terminal.
type runFinish struct {
	status string
	chain  bool
	output any
	params map[string]string
	// duration is the executed wall time reported to the run:finished hooks.
	// Zero for a run finalized without ever executing.
	duration time.Duration
}

// runKindOf maps a task kind onto the run kind persisted in the run row.
func runKindOf(k task.Kinded) string {
	if k.KindOf() == task.KindPipelineTask {
		return registry.RunKindPipeline
	}
	return registry.RunKindTask
}

// beginRun opens a Run: it consults the fire guard, creates the run row, emits
// the run_triggered audit event, persists the trigger payload, fires the
// run:started hook, and registers the run's cancellation, trigger source,
// completion channel and chain depth.
//
// opts.RunID is honored when set — startDaemon (#470) reserves the daemonRuns
// slot before the run goroutine can exit, so it must know the ID up front — and
// generated otherwise. parent roots the returned run context, so a caller with a
// deadline (fireSync's prereq ceiling) propagates it and one without passes
// context.Background().
func (e *Engine) beginRun(parent context.Context, k task.Kinded, opts *pkgruntime.RunOptions, source registry.TriggerSource) (context.Context, *runHandle, error) {
	taskID := k.TaskID()
	kind := runKindOf(k)

	if err := e.checkFireGuard(taskID); err != nil {
		return nil, nil, err
	}
	if opts.RunID == "" {
		opts.RunID = uuid.New().String()
	}
	if _, err := e.registry.StartRunWithID(context.Background(), opts.RunID, taskID, opts.ParentRunID, string(source), kind); err != nil {
		return nil, nil, fmt.Errorf("start run record: %w", err)
	}

	actorID := opts.TriggerActor
	if actorID == "" {
		actorID = opts.ParentRunID
	}
	e.audit.Emit(context.Background(), audit.Event{
		EventType:  audit.EventRunTriggered,
		ActorKind:  string(source),
		ActorID:    actorID,
		TargetKind: kind,
		TargetID:   taskID,
		Params:     audit.SanitizeParams(opts.Params),
		RunID:      opts.RunID,
		Allowed:    true,
	})

	e.persistRunInput(k, opts, source)

	if h := e.runStartedHook; h != nil {
		h(taskID, opts.RunID, string(source))
	}

	runCtx, cancel := context.WithCancel(parent)
	e.runCancels.Store(opts.RunID, cancel)
	e.runTriggerSource.Store(opts.RunID, source)
	e.runDone.Store(opts.RunID, make(chan struct{}))
	if d, ok := chainDepthOf(opts, source); ok {
		e.runChainDepth.Store(opts.RunID, d)
	}

	return runCtx, &runHandle{
		e:               e,
		taskID:          taskID,
		kind:            kind,
		runID:           opts.RunID,
		source:          source,
		chainParentTask: opts.ChainParentTask,
		cancel:          cancel,
		startedAt:       time.Now(),
	}, nil
}

// chainDepthOf reads a fire's chain hop count from RunOptions.ChainDepth, which
// every chain dispatch sets.
//
// A replay is the one source that cannot: it re-fires a run from its persisted
// input, and for a chain-fired run the depth inside that payload is the only
// surviving record of where the run sat in the chain. The envelope is read for
// that source alone — on a webhook fire opts.Input is the request body, and a
// caller must not be able to set the ceiling that bounds its own run's chain.
// Persisted values round-trip through JSON, so the depth can arrive as any
// numeric type; a non-positive one is no hop count and is ignored.
func chainDepthOf(opts *pkgruntime.RunOptions, source registry.TriggerSource) (int, bool) {
	if opts.ChainDepth > 0 {
		return opts.ChainDepth, true
	}
	if source != registry.TriggerReplay {
		return 0, false
	}
	m, ok := opts.Input.(map[string]any)
	if !ok {
		return 0, false
	}
	var depth int
	switch v := m["_chain_depth"].(type) {
	case int:
		depth = v
	case int64:
		depth = int(v)
	case float64:
		depth = int(v)
	default:
		return 0, false
	}
	if depth <= 0 {
		return 0, false
	}
	return depth, true
}

// persistRunInput stores the fire's trigger payload against the run row so the
// UI can show what fired it, and records the blob's key and size on the row.
// Best-effort throughout: a failure never blocks the run.
func (e *Engine) persistRunInput(k task.Kinded, opts *pkgruntime.RunOptions, source registry.TriggerSource) {
	if e.inputStore == nil {
		return
	}
	persist, bodyFullTextual := e.runInputPolicy(k)
	if !persist {
		return
	}

	var web *registry.WebhookFields
	if opts.WebhookCtx != nil {
		web = &registry.WebhookFields{
			Method:          opts.WebhookCtx.Method,
			Path:            opts.WebhookCtx.Path,
			Headers:         opts.WebhookCtx.Headers,
			Query:           opts.WebhookCtx.Query,
			RawBody:         opts.WebhookCtx.RawBody,
			ContentType:     opts.WebhookCtx.ContentType,
			BodyFullTextual: bodyFullTextual,
		}
	}
	in := registry.BuildPersistedInputFromRunOpts(string(source), opts.Params, opts.Input, web)
	key, size, storedAt, err := e.inputStore.Persist(context.Background(), opts.RunID, in)
	if err != nil {
		if errors.Is(err, registry.ErrStorageTaskNotRegistered) {
			// Startup race (#523): daemon-triggered runs (tray, relay-*,
			// nginx-start) fire before buildin/local-storage registers, so the
			// backing store isn't ready yet. Expected and self-healing.
			e.log.Debug("run-input persist skipped: storage task not yet registered",
				zap.String("run", opts.RunID),
				zap.String("task", k.TaskID()),
			)
			return
		}
		// Log only a sanitized error category. The full error chain may transit
		// env-resolver internals carrying a secretKey taint label, which makes
		// CodeQL's go/clear-text-logging fire on the raw value.
		e.log.Warn("run-input persist failed",
			zap.String("run", opts.RunID),
			zap.String("task", k.TaskID()),
			zap.String("error_class", "persist"),
		)
		return
	}
	if opts.WebhookCtx != nil {
		// Bound RAM exposure: RawBody is no longer needed now that the blob has
		// been persisted. Nil it out so the slice can be GC'd rather than held
		// for the full run lifetime.
		opts.WebhookCtx.RawBody = nil
	}
	if err := e.registry.SetRunInput(context.Background(), opts.RunID, key, size, storedAt, in.RedactedFields); err != nil {
		e.log.Warn("run-input set columns failed",
			zap.String("run", opts.RunID),
			zap.String("task", k.TaskID()),
			zap.Error(err),
		)
	}
}

// runInputPolicy answers, for one task kind, whether a fire's trigger payload
// is persisted and whether a webhook body is stored in full textual form.
//
// kind: PipelineTask carries no run_inputs block and can be neither the
// storage task nor the cleanup task, so both of shouldPersistInput's recursion
// guards and its opt-out are vacuous for it.
func (e *Engine) runInputPolicy(k task.Kinded) (persist, bodyFullTextual bool) {
	spec, ok := k.(*task.Spec)
	if !ok {
		return true, false
	}
	if spec.RunInputs != nil && spec.RunInputs.BodyFullTextual != nil {
		bodyFullTextual = *spec.RunInputs.BodyFullTextual
	}
	return e.shouldPersistInput(spec), bodyFullTextual
}

// finish closes the Run: it logs the outcome, fires the run:finished hooks,
// wakes WaitRun, dispatches downstream chain edges, and releases every
// registration keyed on the run ID. The caller has already made the run row
// terminal.
func (h *runHandle) finish(f runFinish) {
	h.once.Do(func() {
		e := h.e
		fields := []zap.Field{
			zap.String("task", h.taskID),
			zap.String("run", h.runID),
			zap.String("kind", h.kind),
			zap.String("status", f.status),
			zap.String("trigger", string(h.source)),
			zap.Duration("duration", f.duration.Truncate(time.Millisecond)),
		}
		if f.status == registry.StatusSuccess {
			e.log.Debug("run finished", fields...)
		} else {
			e.log.Warn("run finished", fields...)
		}

		e.emitRunFinished(h.taskID, h.runID, f.status, string(h.source), f.duration.Milliseconds())

		// Wake WaitRun before dispatching chains: the row is already terminal,
		// so a woken caller reads the finalized result rather than waiting out
		// a synchronous chain fan-out.
		if v, ok := e.runDone.LoadAndDelete(h.runID); ok {
			close(v.(chan struct{}))
		}
		if f.chain {
			// Ahead of the teardown below, which drops the trigger source
			// FireChain's replay guard consults and the chain depth its
			// ceiling reads.
			e.FireChain(context.Background(), h.taskID, h.runID, f.status, f.output, f.params)
		}
		h.release()
	})
}

// elapsed is how long the Run has been open, measured from the run row's
// insert.
func (h *runHandle) elapsed() time.Duration { return time.Since(h.startedAt) }

// teardown releases the Run's registrations without firing hooks or chains.
// It is the deferred backstop for a body that never reached a terminal outcome
// — after a normal finish it is a no-op.
func (h *runHandle) teardown() {
	h.once.Do(h.release)
}

// release drops every registration keyed on the run ID. Callers hold once.
func (h *runHandle) release() {
	e := h.e
	if h.chainParentTask != "" {
		e.guards.releaseSlot(h.chainParentTask)
	}
	e.runCancels.Delete(h.runID)
	e.runTriggerSource.Delete(h.runID)
	e.runChainDepth.Delete(h.runID)
	if h.cancel != nil {
		h.cancel()
	}
	if v, ok := e.runDone.LoadAndDelete(h.runID); ok {
		close(v.(chan struct{}))
	}
	// Defer deletion of the suppressed-persistence return-value cache: WaitRun
	// goroutines woken by the runDone close need time to scan runReturnValue
	// before the entry is removed. The map is only populated for
	// `run_result.enabled: false` tasks, so this is a no-op for the common case.
	runID := h.runID
	time.AfterFunc(runReturnValueTTL, func() { e.runReturnValue.Delete(runID) })
}

// emitRunFinished fans a terminal run out to every registered run:finished
// hook. The suspension sweep calls it without a handle: by the time a
// suspension's deadline expires, the run's goroutine is long gone.
func (e *Engine) emitRunFinished(taskID, runID, status, source string, durationMs int64) {
	for _, h := range e.runFinishedHooks {
		h(taskID, runID, status, source, durationMs)
	}
}
