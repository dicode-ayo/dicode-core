// This file contains run execution: the manual fire entry points, run
// bookkeeping (start records, cleanup, cancellation, waiting), the
// fireKinded/fireAsync/fireSync fire paths, if_missing prereq resolution,
// and executor dispatch.

package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dicode/dicode/pkg/audit"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// ErrRunNotFound is returned by WaitRun when no run record exists for the given ID.
var ErrRunNotFound = errors.New("run not found")

// runReturnValueTTL is how long a suppressed-persistence return value
// stays in the in-memory runReturnValue map after the run reaches a
// terminal state. Long enough for a WaitRun caller woken by the runDone
// close to scan the map; short enough that orphaned entries (no waiter)
// don't accumulate. Only `run_result.enabled: false` tasks populate the
// map, so this is a small footprint either way.
const runReturnValueTTL = 30 * time.Second

// FireManual triggers a task by ID with optional param overrides.
func (e *Engine) FireManual(ctx context.Context, taskID string, params map[string]string) (string, error) {
	return e.FireManualWithActor(ctx, taskID, params, "")
}

// FireManualWithActor is FireManual carrying the identity of the operator
// principal (e.g. the authenticated web session's client IP) that requested
// the run. The actor flows into the run_triggered audit event (#45) as
// actor_id; an empty actor preserves plain FireManual behaviour.
func (e *Engine) FireManualWithActor(ctx context.Context, taskID string, params map[string]string, actor string) (string, error) {
	k, ok := e.registry.GetKinded(taskID)
	if !ok {
		return "", fmt.Errorf("task %q not found", taskID)
	}
	e.log.Info("manual trigger", zap.String("task", taskID))
	return e.fireKinded(context.Background(), k, pkgruntime.RunOptions{Params: params, TriggerActor: actor}, registry.TriggerManual)
}

// FireFromTask triggers a task as a child of an in-flight run. Used by the
// dicode.run_task IPC handler so the new run's parent_run_id (#116) points
// at the caller. Falls back to a plain manual fire when parentRunID is "".
func (e *Engine) FireFromTask(ctx context.Context, taskID, parentRunID string, params map[string]string) (string, error) {
	if parentRunID == "" {
		return e.FireManual(ctx, taskID, params)
	}
	k, ok := e.registry.GetKinded(taskID)
	if !ok {
		return "", fmt.Errorf("task %q not found", taskID)
	}
	e.log.Info("subtask trigger", zap.String("task", taskID), zap.String("parent", parentRunID))
	return e.fireKinded(context.Background(), k,
		pkgruntime.RunOptions{Params: params, ParentRunID: parentRunID},
		registry.TriggerManual)
}

// WaitRun blocks until the run identified by runID reaches a terminal state,
// then returns a RunResult. Implements ipc.EngineRunner.
//
// Channel lifecycle: startRun() registers a chan struct{} in runDone keyed by
// runID. The cleanup func (deferred in fireAsync/fireSync via startRun) closes
// that channel once the run goroutine finishes. WaitRun selects on the channel
// so it is woken up immediately rather than polling.
//
// Race: if WaitRun is called after the channel has already been closed and
// deleted (i.e. the run finished before the caller reached this function), the
// Load will return ok==false. In that case we fall through to a single DB read
// to return the final status.
func (e *Engine) WaitRun(ctx context.Context, runID string) (ipc.RunResult, error) {
	if v, ok := e.runDone.Load(runID); ok {
		// Run is in progress — wait for the completion channel to be closed.
		select {
		case <-v.(chan struct{}):
			// Channel closed: run has finished. Fall through to DB read below.
		case <-ctx.Done():
			return ipc.RunResult{}, ctx.Err()
		}
	}
	// Either the channel was never present (run already finished before we
	// arrived) or it was just closed. Either way, fetch the final record.
	run, err := e.registry.GetRun(ctx, runID)
	if err != nil {
		if errors.Is(err, registry.ErrRunNotFound) {
			return ipc.RunResult{}, ErrRunNotFound
		}
		return ipc.RunResult{}, err
	}
	// Prefer the persisted return_value when present. Fall back to the
	// in-memory runReturnValue cache for tasks that opted out of
	// persistence via `run_result.enabled: false` — without this fallback,
	// `dicode.run_task` callers would receive nil for those tasks even
	// though the value is available in process. The cache entry survives
	// for runReturnValueTTL after run completion (see startRun cleanup).
	returnJSON := run.ReturnValue
	if returnJSON == "" {
		if v, ok := e.runReturnValue.Load(runID); ok {
			returnJSON, _ = v.(string)
		}
	}
	var returnValue interface{}
	if returnJSON != "" {
		_ = json.Unmarshal([]byte(returnJSON), &returnValue)
	}
	return ipc.RunResult{
		RunID:       runID,
		Status:      run.Status,
		ReturnValue: returnValue,
	}, nil
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

// startRun creates the DB record, stores the cancel func, fires the started
// hook, and returns a ready-to-run context. The caller is responsible for
// calling the returned cleanup func when the run finishes.
// startRunWithParent is like startRun but accepts an explicit parent context
// for the run's cancellation context. The run is cancelled when either the
// parent context expires or KillRun is called. Pass context.Background() to
// reproduce the original independent-context behaviour.
func (e *Engine) startRunWithParent(parent context.Context, spec *task.Spec, opts *pkgruntime.RunOptions, source registry.TriggerSource) (runCtx context.Context, cleanup func(), err error) {
	if err = e.checkFireGuard(spec.ID); err != nil {
		return nil, nil, err
	}
	if _, err = e.registry.StartRunWithID(context.Background(), opts.RunID, spec.ID, opts.ParentRunID, string(source), registry.RunKindTask); err != nil {
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
		TargetKind: "task",
		TargetID:   spec.ID,
		Params:     audit.SanitizeParams(opts.Params),
		RunID:      opts.RunID,
		Allowed:    true,
	})

	if e.inputStore != nil && e.shouldPersistInput(spec) {
		var web *registry.WebhookFields
		if opts.WebhookCtx != nil {
			bft := false
			if spec.RunInputs != nil && spec.RunInputs.BodyFullTextual != nil {
				bft = *spec.RunInputs.BodyFullTextual
			}
			web = &registry.WebhookFields{
				Method:          opts.WebhookCtx.Method,
				Path:            opts.WebhookCtx.Path,
				Headers:         opts.WebhookCtx.Headers,
				Query:           opts.WebhookCtx.Query,
				RawBody:         opts.WebhookCtx.RawBody,
				ContentType:     opts.WebhookCtx.ContentType,
				BodyFullTextual: bft,
			}
		}
		in := registry.BuildPersistedInputFromRunOpts(string(source), opts.Params, opts.Input, web)
		key, size, storedAt, perr := e.inputStore.Persist(context.Background(), opts.RunID, in)
		if perr != nil {
			e.log.Warn("run-input persist failed",
				zap.String("run", opts.RunID),
				zap.String("task", spec.ID),
				zap.String("error_class", "persist"),
			)
		} else {
			if opts.WebhookCtx != nil {
				// Bound RAM exposure: RawBody is no longer needed now that the
				// blob has been persisted. Nil it out so the slice can be GC'd
				// rather than held for the full run lifetime.
				opts.WebhookCtx.RawBody = nil
			}
			if serr := e.registry.SetRunInput(context.Background(), opts.RunID, key, size, storedAt, in.RedactedFields); serr != nil {
				e.log.Warn("run-input set columns failed",
					zap.String("run", opts.RunID),
					zap.String("task", spec.ID),
					zap.Error(serr),
				)
			}
		}
	}

	if h := e.runStartedHook; h != nil {
		h(spec.ID, opts.RunID, string(source))
	}
	var cancel context.CancelFunc
	runCtx, cancel = context.WithCancel(parent)
	e.runCancels.Store(opts.RunID, cancel)
	e.runTriggerSource.Store(opts.RunID, source)

	doneCh := make(chan struct{})
	e.runDone.Store(opts.RunID, doneCh)

	cleanup = func() {
		if opts.ChainParentTask != "" {
			e.guards.releaseSlot(opts.ChainParentTask)
		}
		e.runCancels.Delete(opts.RunID)
		e.runTriggerSource.Delete(opts.RunID)
		e.runChainDepth.Delete(opts.RunID)
		cancel()
		if v, ok := e.runDone.LoadAndDelete(opts.RunID); ok {
			close(v.(chan struct{}))
		}
		// Defer deletion of the suppressed-persistence return-value cache:
		// WaitRun goroutines woken by the runDone close above need time to
		// scan runReturnValue before the entry is removed. The map is only
		// populated for `run_result.enabled: false` tasks, so this AfterFunc
		// is a no-op for the common case.
		runID := opts.RunID
		time.AfterFunc(runReturnValueTTL, func() {
			e.runReturnValue.Delete(runID)
		})
	}
	return runCtx, cleanup, nil
}

// startRun creates a run record and context rooted at context.Background() —
// the standard path for all trigger types. See startRunWithParent for the
// variant that wires an explicit parent (used by fireSync so prereq timeouts
// propagate correctly).
func (e *Engine) startRun(spec *task.Spec, opts *pkgruntime.RunOptions, source registry.TriggerSource) (runCtx context.Context, cleanup func(), err error) {
	return e.startRunWithParent(context.Background(), spec, opts, source)
}

// shouldPersistInput returns true when the run's input should be persisted.
// It enforces two recursion guards and respects the per-task opt-out flag.
func (e *Engine) shouldPersistInput(spec *task.Spec) bool {
	// Per-task opt-out: run_inputs.enabled: false in task.yaml.
	if spec.RunInputs != nil && spec.RunInputs.Enabled != nil && !*spec.RunInputs.Enabled {
		return false
	}
	// Recursion guard: never persist the storage task's own inputs.
	if e.inputStore != nil && spec.ID == e.inputStore.StorageTaskID() {
		return false
	}
	// Recursion guard: cleanup task runs periodically with the same retention
	// config; persisting it every cron tick is pointless and loops.
	if spec.ID == "buildin/run-inputs-cleanup" {
		return false
	}
	return true
}

// runTask executes a task synchronously and handles all post-run bookkeeping
// (logging, notifications, hooks, daemon restart). Returns status and result.
func (e *Engine) runTask(runCtx context.Context, spec *task.Spec, opts pkgruntime.RunOptions, source registry.TriggerSource) (string, *pkgruntime.RunResult) {
	e.log.Info("run started",
		zap.String("task", spec.ID),
		zap.String("run", opts.RunID),
		zap.String("trigger", string(source)),
		zap.String("runtime", string(spec.Runtime)),
	)

	start := time.Now()

	// Preflight env-resolver: typed envresolve failures
	// (provider_unavailable / required_secret_missing / provider_misconfigured)
	// are recorded as the run's fail_reason BEFORE dispatch so the operator
	// gets a categorized reason instead of an opaque executor error.
	//
	// On preflight success the *Resolved is forwarded to the runtime via
	// opts.PreResolvedEnv so provider tasks fire exactly once per consumer
	// launch (issue #235). When preflight is skipped (no secrets chain /
	// no env entries) preResolved is nil and the runtime falls back to its
	// inline-resolver path.
	var status string
	var result *pkgruntime.RunResult
	preResolved, preStatus, preReason := e.preflightEnv(runCtx, spec)
	if preStatus != "" {
		if err := e.registry.FinishRunWithReason(context.Background(), opts.RunID, preStatus, preReason); err != nil {
			e.log.Warn("FinishRun: preflight failure", zap.String("run", opts.RunID), zap.Error(err))
		}
		// Chain-on-failure semantics: dispatch normally fires FireChain;
		// the preflight-env short-circuit replicates it so chain triggers
		// and on_failure_chain still observe the failure.
		//
		// Called synchronously so the caller's deferred cleanup() (which
		// deletes runChainDepth[opts.RunID]) cannot race ahead of FireChain's
		// depth lookup. An earlier draft used `go FireChain(...)` to mirror a
		// fire-and-forget shape, but that allowed cleanup to observe a
		// depth-of-zero and let a chain-fired parent take one hop past the
		// MaxDepth ceiling (issue #334, sister to #331). Matches the normal
		// FireChain call site at the end of dispatch, which is also
		// synchronous.
		e.FireChain(context.Background(), spec.ID, opts.RunID, preStatus, nil, opts.Params)
		status = preStatus
		result = &pkgruntime.RunResult{RunID: opts.RunID, Error: errors.New(preReason)}
	} else {
		opts.PreResolvedEnv = preResolved
		status, result = e.dispatch(runCtx, spec, opts)
	}
	elapsed := time.Since(start)

	runFields := []zap.Field{
		zap.String("task", spec.ID),
		zap.String("run", opts.RunID),
		zap.String("status", status),
		zap.String("trigger", string(source)),
		zap.Duration("duration", elapsed.Truncate(time.Millisecond)),
	}
	if status == registry.StatusSuccess {
		e.log.Debug("run finished", runFields...)
	} else {
		e.log.Warn("run finished", runFields...)
	}

	if h := e.runFinishedHook; h != nil {
		h(spec.ID, opts.RunID, status, string(source), elapsed.Milliseconds())
	}

	// Drive the standalone-daemon lifecycle (restart-policy decision,
	// DaemonState transition) only for runs that the engine actually manages as
	// standalone daemons. A pipeline's terminal stage is dispatched as a
	// daemon-kind run too (source=pipeline-stage), but it is owned by the
	// PipelineRunner, not the standalone-daemon machinery: its lifetime,
	// restarts, and DaemonState are not tracked in daemonRuns/daemonStates.
	// Routing a pipeline-stage daemon run through onDaemonRunFinished would,
	// when the stage task ALSO happens to be a registered standalone daemon,
	// flip that standalone daemon's global DaemonState and possibly schedule an
	// unintended startDaemon on a mid-pipeline restart's KillRun (#344, LOW
	// security). Suppress the hook for pipeline-stage runs. This gate depends on
	// fireStageRaw always dispatching pipeline stages with TriggerPipelineStage
	// (see the INVARIANT note at pipeline_runner.go:fireStageRaw's fireAsync).
	if spec.Trigger.Daemon && source != registry.TriggerPipelineStage {
		e.onDaemonRunFinished(spec, opts.RunID)
	}

	return status, result
}

// finalizeCancelled closes out a run that was cancelled before ever executing
// — e.g. user killed it while queued on the concurrency semaphore, or the
// daemon is shutting down. It updates the registry row and fires the finished
// hook so websocket subscribers see a matching started/finished pair.
// Safe to call even on the hot shutdown path: the DB write is bounded by a
// short timeout so a stuck SQLite connection cannot block the goroutine.
func (e *Engine) finalizeCancelled(spec *task.Spec, opts pkgruntime.RunOptions, source registry.TriggerSource) {
	finishCtx, finishCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer finishCancel()
	if err := e.registry.FinishRun(finishCtx, opts.RunID, registry.StatusCancelled); err != nil {
		e.log.Error("failed to finalize cancelled queued run",
			zap.String("run", opts.RunID),
			zap.String("task", spec.ID),
			zap.Error(err))
	}
	if h := e.runFinishedHook; h != nil {
		// Duration is 0 — the run never executed.
		h(spec.ID, opts.RunID, registry.StatusCancelled, string(source), 0)
	}
}

// fireAsync pre-creates the run record, starts execution in a goroutine,
// and returns the run ID immediately.
//
// When a MaxConcurrentTasks semaphore is configured, the goroutine blocks
// until a slot is available or the shutdown context is cancelled — ensuring
// shutdown never deadlocks waiting tasks.
//
// fireKinded dispatches any task kind, returning the (parent) run ID: a
// *task.PipelineTask runs via the PipelineRunner (firePipeline); a *task.Spec
// runs via fireAsync. Trigger entrypoints (manual/cron/webhook/chain) route
// through here so a pipeline and a plain task fire uniformly.
func (e *Engine) fireKinded(ctx context.Context, k task.Kinded, opts pkgruntime.RunOptions, source registry.TriggerSource) (string, error) {
	if err := e.checkFireGuard(k.TaskID()); err != nil {
		return "", err
	}
	switch s := k.(type) {
	case *task.PipelineTask:
		return e.firePipeline(ctx, s, opts, source)
	case *task.Spec:
		return e.fireAsync(ctx, s, opts, source)
	default:
		return "", fmt.Errorf("engine: cannot fire unsupported kind %q", k.KindOf())
	}
}

func (e *Engine) fireAsync(ctx context.Context, spec *task.Spec, opts pkgruntime.RunOptions, source registry.TriggerSource) (string, error) {
	// Honor a caller-provided run ID; generate one only when absent. The only
	// caller that pre-sets opts.RunID is startDaemon (#470): it must reserve
	// the daemonRuns slot BEFORE the run goroutine can exit, which requires
	// knowing the run ID before fireAsync launches the body. Every other call
	// site passes a fresh RunOptions with an empty RunID.
	if opts.RunID == "" {
		opts.RunID = uuid.New().String()
	}

	runCtx, cleanup, err := e.startRun(spec, &opts, source)
	if err != nil {
		return "", err
	}

	// If this run carries a _chain_depth in its input (set by FireChain), record
	// it in runChainDepth so FireChain can read the depth when this run completes.
	if m, ok := opts.Input.(map[string]any); ok {
		if d, ok2 := m["_chain_depth"]; ok2 {
			switch v := d.(type) {
			case int:
				e.runChainDepth.Store(opts.RunID, v)
			case int64:
				e.runChainDepth.Store(opts.RunID, int(v))
			case float64:
				e.runChainDepth.Store(opts.RunID, int(v))
			}
		}
	}

	// Add before `go` so a shutdown drain cannot race past a not-yet-started run.
	e.runWG.Add(1)
	go func() {
		// First deferred statement → runs last (LIFO), after cleanup and all
		// finalization, so the drain releases only once the DB writes are done.
		defer e.runWG.Done()
		defer cleanup()

		// Daemon tasks are long-running; they must not consume semaphore slots
		// or they would permanently starve webhook/cron tasks.
		if e.taskSem != nil && !spec.Trigger.Daemon {
			// Build a done channel that is nil (blocks forever) when the engine
			// has not yet started — nil channels never select, which is safe.
			var shutDone <-chan struct{}
			shutCtx := e.getShutdownCtx()
			if shutCtx != nil {
				shutDone = shutCtx.Done()
			}

			// Increment the "parked waiting for a slot" counter. Each select
			// case below must decrement exactly once: "waiting" ends when
			// either the slot is acquired or the goroutine aborts — NOT
			// when the run itself finishes. A single deferred decrement
			// here would wrongly keep the counter inflated for the entire
			// lifetime of successfully-acquired runs.
			e.taskWaiting.Add(1)
			select {
			case e.taskSem <- struct{}{}:
				e.taskWaiting.Add(-1)
				// Slot acquired; release when done.
				defer func() { <-e.taskSem }()
			case <-runCtx.Done():
				// User killed (or gave up on) the queued run before a slot
				// freed. Finalize it as cancelled so the websocket finished
				// event fires and the DB row doesn't stay stuck in `running`.
				e.taskWaiting.Add(-1)
				e.finalizeCancelled(spec, opts, source)
				return
			case <-shutDone:
				// Engine is shutting down; finalize as cancelled and abort.
				e.taskWaiting.Add(-1)
				e.finalizeCancelled(spec, opts, source)
				return
			}
		}

		e.runTask(runCtx, spec, opts, source)
	}()

	return opts.RunID, nil
}

// fireSync runs the task synchronously and returns the run ID and result.
// The caller's context is used only for cancellation of the run setup; the
// run itself uses an independent context so it is not cancelled when the HTTP
// request context ends mid-execution.
//
// When callerCtx is non-nil, it is used as the parent of the run context so
// a deadline on callerCtx (e.g. the prereq ceiling from resolveIfMissing)
// propagates to the run. Pass context.Background() to preserve the old
// independent-context behaviour.
func (e *Engine) fireSync(callerCtx context.Context, spec *task.Spec, opts pkgruntime.RunOptions, source registry.TriggerSource) (string, *pkgruntime.RunResult, error) {
	opts.RunID = uuid.New().String()

	runCtx, cleanup, err := e.startRunWithParent(callerCtx, spec, &opts, source)
	if err != nil {
		return "", nil, err
	}
	defer cleanup()

	status, result := e.runTask(runCtx, spec, opts, source)
	if result == nil {
		result = &pkgruntime.RunResult{}
	}
	if result.Error == nil && status != registry.StatusSuccess {
		result.Error = fmt.Errorf("run %s", status)
	}
	return opts.RunID, result, nil
}

// ifMissingPrereqTimeout bounds how long resolveIfMissing will wait for a
// single prereq task to complete. The prereq's own spec-level Timeout is
// respected first; this is an engine-level ceiling so a wedged prereq (slow
// DNS, stuck upstream, etc.) can't block the webhook caller indefinitely.
const ifMissingPrereqTimeout = 60 * time.Second

// ifMissingErrorFormat is the prefix used by the errors resolveIfMissing
// returns when a prereq is missing/failed. UI consumers (notably
// tasks/buildin/ai-agent/chat.js `detectSetup`) regex-match this prefix
// to render a "Set up <Provider>" card instead of a raw error log.
//
// STABLE INTERFACE — do not rephrase without updating every consumer.
//
//	if_missing: secret %q requires setup via task %q[: wrapped-error]
//	if_missing: secret %q requires setup via task %q (ran but still unset)[: wrapped-error]
const ifMissingErrorFormat = `if_missing: secret %q requires setup via task %q`

// resolveIfMissing scans the task's env entries for `if_missing` directives
// and, for any whose target secret is not present in the secrets chain,
// synchronously runs the declared prereq task in chain mode. If the prereq
// succeeds and the secret is now resolvable, dispatch continues normally.
// If the prereq fails, its error — typically an OAuth flow's "open this URL
// to authorize" message — bubbles up as the original task's failure so the
// UI can surface a setup call-to-action (see ifMissingErrorFormat).
//
// Concurrency: parallel calls with the same secret are collapsed via
// singleflight, so N webhook calls with the same missing secret yield one
// prereq run, not N. The shared result is returned to all callers — each
// then re-resolves the secret independently to decide whether to proceed.
//
// Timeout: the prereq's own spec Timeout applies first; an engine-level
// ifMissingPrereqTimeout (60s) bounds total wait to protect the HTTP caller
// even if a misconfigured prereq declared a higher timeout.
//
// No-ops when: secrets chain is not wired, the entry has no `if_missing`
// directive, or the secret already resolves. Non-secret-backed entries
// (From/Value/bare) are ignored — if_missing only applies to `secret:`.
func (e *Engine) resolveIfMissing(ctx context.Context, spec *task.Spec, parentRunID string) error {
	if e.secrets == nil {
		return nil
	}
	for _, entry := range spec.Permissions.Env {
		if entry.IfMissing == nil || entry.IfMissing.Task == "" || entry.Secret == "" {
			continue
		}
		if _, err := e.secrets.Resolve(ctx, entry.Secret); err == nil {
			continue // secret already present
		} else {
			var notFound *secrets.NotFoundError
			if !errors.As(err, &notFound) {
				return fmt.Errorf("check secret %q for env %q: %w", entry.Secret, entry.Name, err)
			}
		}

		prereqID := entry.IfMissing.Task
		prereqSpec, ok := e.registry.Get(prereqID)
		if !ok {
			return fmt.Errorf("if_missing task %q for secret %q is not registered", prereqID, entry.Secret)
		}

		e.log.Info("if_missing: running prereq",
			zap.String("task", spec.ID),
			zap.String("secret", entry.Secret),
			zap.String("prereq", prereqID),
		)

		prereqCtx, cancel := context.WithTimeout(ctx, ifMissingPrereqTimeout)
		params := entry.IfMissing.Params
		sfKey := "if_missing:" + entry.Secret
		res, sfErr, _ := e.prereqFlight.Do(sfKey, func() (any, error) {
			// Chain-mode input: non-nil empty map so the prereq task's
			// `input !== null` check treats this as a programmatic
			// invocation (silent refresh path), not an interactive UI click.
			chainInput := map[string]any{}
			// Pass prereqCtx so the 60s engine-level ceiling is enforced even
			// if the prereq task's own spec Timeout is longer (Fix 2, #387).
			prereqRunID, result, fireErr := e.fireSync(prereqCtx, prereqSpec, pkgruntime.RunOptions{
				ParentRunID: parentRunID,
				Input:       chainInput,
				Params:      params,
			}, "if_missing")
			return prereqFlightResult{runID: prereqRunID, result: result}, firstNonNil(fireErr, prereqCtx.Err())
		})
		cancel()

		if sfErr != nil {
			return fmt.Errorf("fire if_missing task %q: %w", prereqID, sfErr)
		}
		pfr := res.(prereqFlightResult)
		if pfr.result != nil && pfr.result.Error != nil {
			// Replay the prereq's logs into the parent run so the webhook
			// response surfaces whatever the prereq printed — typically an
			// "Open this URL to authorize" line the chat UI can render as a
			// clickable setup link. Without this, callers only see
			// "exit status 1" with no actionable context.
			e.copyPrereqLogs(ctx, pfr.runID, parentRunID, prereqID)
			return fmt.Errorf(ifMissingErrorFormat+": %w", entry.Secret, prereqID, pfr.result.Error)
		}

		if _, err := e.secrets.Resolve(ctx, entry.Secret); err != nil {
			e.copyPrereqLogs(ctx, pfr.runID, parentRunID, prereqID)
			return fmt.Errorf(ifMissingErrorFormat+" (ran but still unset): %w", entry.Secret, prereqID, err)
		}
	}
	return nil
}

// prereqFlightResult is the shared payload stored in singleflight so all
// callers waiting on the same secret get the same (runID, result) pair.
type prereqFlightResult struct {
	runID  string
	result *pkgruntime.RunResult
}

// firstNonNil returns the first non-nil error, letting a context-cancel
// surface from resolveIfMissing's timeout even when fireSync itself
// returned a nil error (because the executor observed the context fine).
func firstNonNil(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// copyPrereqLogs fetches a prereq run's log entries and appends them to the
// parent run's log, prefixed so they're visibly attributed. Best-effort:
// logging is a diagnostic aid and must never break the calling path.
func (e *Engine) copyPrereqLogs(ctx context.Context, prereqRunID, parentRunID, prereqID string) {
	if parentRunID == "" || prereqRunID == "" {
		return
	}
	logs, err := e.registry.GetRunLogs(ctx, prereqRunID)
	if err != nil {
		return
	}
	prefix := "[prereq " + prereqID + "] "
	for _, le := range logs {
		_ = e.registry.AppendLog(ctx, parentRunID, le.Level, prefix+le.Message)
	}
}

// dispatch routes a run to the appropriate executor and returns the final status and result.
func (e *Engine) dispatch(ctx context.Context, spec *task.Spec, opts pkgruntime.RunOptions) (string, *pkgruntime.RunResult) {
	e.mu.Lock()
	exec, ok := e.executors[spec.Runtime]
	e.mu.Unlock()

	if !ok || exec == nil {
		e.log.Error("no executor for runtime",
			zap.String("task", spec.ID),
			zap.String("runtime", string(spec.Runtime)),
		)
		if err := e.registry.FinishRun(context.Background(), opts.RunID, registry.StatusFailure); err != nil {
			e.log.Warn("FinishRun: no executor", zap.String("run", opts.RunID), zap.Error(err))
		}
		return registry.StatusFailure, &pkgruntime.RunResult{Error: fmt.Errorf("no executor for runtime %s", spec.Runtime)}
	}

	if err := e.resolveIfMissing(ctx, spec, opts.RunID); err != nil {
		e.log.Warn("if_missing prereq unsatisfied",
			zap.String("task", spec.ID),
			zap.Error(err),
		)
		if err2 := e.registry.FinishRun(context.Background(), opts.RunID, registry.StatusFailure); err2 != nil {
			e.log.Warn("FinishRun: if_missing failure", zap.String("run", opts.RunID), zap.Error(err2))
		}
		return registry.StatusFailure, &pkgruntime.RunResult{Error: err}
	}

	result, err := exec.Execute(ctx, spec, opts)
	if err != nil {
		e.log.Error("executor error",
			zap.String("task", spec.ID),
			zap.String("runtime", string(spec.Runtime)),
			zap.Error(err),
		)
		if err2 := e.registry.FinishRun(context.Background(), opts.RunID, registry.StatusFailure); err2 != nil {
			e.log.Warn("FinishRun: executor error", zap.String("run", opts.RunID), zap.Error(err2))
		}
		return registry.StatusFailure, &pkgruntime.RunResult{Error: err}
	}

	// dicode.suspend() paused the task (#95): persist the run as suspended with
	// a fresh resume token and deadline instead of finishing it, and skip the
	// chain — suspended is non-terminal, so success/failure chains must not
	// fire until the continuation run reaches a real terminal state.
	if result.Suspended {
		if serr := e.suspendRun(&opts, result); serr != nil {
			e.log.Warn("suspend run persist failed", zap.String("run", opts.RunID), zap.Error(serr))
			if ferr := e.registry.FinishRun(context.Background(), opts.RunID, registry.StatusFailure); ferr != nil {
				e.log.Warn("FinishRun: suspend fallback", zap.String("run", opts.RunID), zap.Error(ferr))
			}
			return registry.StatusFailure, &pkgruntime.RunResult{Error: serr}
		}
		return registry.StatusSuspended, result
	}

	// Compute final status from the result.
	status := registry.StatusSuccess
	if result.Error != nil {
		if ctx.Err() != nil {
			status = registry.StatusCancelled
		} else {
			status = registry.StatusFailure
		}
	}

	// Marshal return value and determine persistence.
	//
	// `run_result.enabled: false` opts out of persisting the JSON-marshalled
	// return value to `runs.return_value`. Structured output_content and
	// output_content_type are unaffected — those carry rich-output payloads
	// (images, HTML) that are addressed separately by the WebUI's content
	// type negotiation and aren't part of the confidentiality concern.
	//
	// In-memory delivery: regardless of persistence, the marshalled JSON is
	// stashed in runReturnValue so WaitRun can serve it to synchronous
	// callers (dicode.run_task -> IPC reply). The entry is cleared by the
	// startRun cleanup func once the run reaches a terminal state.
	retJSON := ""
	if result != nil && result.ReturnValue != nil {
		if b, merr := json.Marshal(result.ReturnValue); merr == nil {
			retJSON = string(b)
		}
	}

	persistedReturnJSON := retJSON
	if persistReturnValue := spec.RunResult.PersistReturnValue(); !persistReturnValue {
		persistedReturnJSON = ""
		if retJSON != "" {
			e.runReturnValue.Store(opts.RunID, retJSON)
		}
	}

	outputContentType := ""
	outputContent := ""
	if result != nil {
		outputContentType = result.OutputContentType
		outputContent = result.OutputContent
	}

	// Atomic: set status + finished_at + return_value + output in one UPDATE.
	// This eliminates the race where a reader polling for status != running
	// could see the status flip before return_value was written.
	if err := e.registry.FinishRunWithResult(context.Background(), opts.RunID, status,
		persistedReturnJSON, outputContentType, outputContent); err != nil {
		e.log.Warn("FinishRun: finalize run result", zap.String("run", opts.RunID), zap.Error(err))
	}

	e.FireChain(context.Background(), spec.ID, opts.RunID, status, result.ChainInput, opts.Params)
	return status, result
}
