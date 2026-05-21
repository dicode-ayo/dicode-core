package trigger

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/taskset"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// PipelineRunner orchestrates one fire of a kind: PipelineTask. It owns the
// pipeline's parent run row and drives its stages sequentially. One runner per
// fire.
//
// When the terminal stage is a daemon, the runner becomes "live" for the
// daemon's lifetime: it registers itself in Engine.livePipelines and records
// the terminal stage's position, the upstream context that fed it, and the
// current daemon run ID under mu (mu guards them because a future mid-pipeline
// re-fire path — PR4 Task 19 — reads them from another goroutine).
type PipelineRunner struct {
	engine *Engine
	spec   *task.PipelineTask
	runID  string             // the pipeline's own parent run ID
	cancel context.CancelFunc // cancels runCtx; invoked on finish + by KillRun
	runCtx context.Context    // the pipeline's lifecycle context (cancelled by KillRun/finish)

	mu sync.Mutex
	// live-daemon-terminal-stage state, set once the terminal daemon stage is
	// fired. All guarded by mu.
	terminalIdx      int               // index of the terminal stage in spec.Stages
	terminalStage    task.Stage        // the terminal daemon stage
	terminalUpstream task.InputContext // the upstream ${input} that fed the terminal stage
	daemonRunID      string            // current terminal daemon stage run ID
	finished         bool              // set by finish()
}

// firePipeline creates the pipeline's parent run row (kind=pipeline) and starts
// the runner asynchronously, returning the parent run ID immediately (mirrors
// fireAsync's contract for kind: Task).
//
// It re-validates the pipeline against the live registry before creating any
// run row. Manual and chain fires reach this path via GetKinded without going
// through engine registration, so a pipeline whose stages were deregistered (or
// that lost the reconcile-ordering race, see #341) is rejected loudly here
// rather than dispatched and failed mid-flight.
func (e *Engine) firePipeline(ctx context.Context, p *task.PipelineTask, opts pkgruntime.RunOptions, source registry.TriggerSource) (string, error) {
	if err := e.validatePipelineRefs(p); err != nil {
		return "", fmt.Errorf("pipeline %q failed validation: %w", p.ID, err)
	}
	runID := uuid.New().String()
	if _, err := e.registry.StartRunWithID(context.Background(), runID, p.ID, opts.ParentRunID, string(source), registry.RunKindPipeline); err != nil {
		return "", fmt.Errorf("start pipeline run: %w", err)
	}
	// Register the parent in the engine's run-lifecycle maps so it behaves like
	// any managed run: WaitRun blocks on runDone until finish() (so a
	// dicode.run_task targeting a pipeline gets the real result, not a racy nil),
	// and KillRun(parentRunID) cancels runCtx — which the runner propagates to
	// the in-flight stage. Trigger entrypoints pass a background context, so the
	// async pipeline survives the trigger call returning.
	runCtx, cancel := context.WithCancel(ctx)
	e.runCancels.Store(runID, cancel)
	e.runTriggerSource.Store(runID, source)
	e.runDone.Store(runID, make(chan struct{}))
	r := &PipelineRunner{engine: e, spec: p, runID: runID, cancel: cancel, runCtx: runCtx}
	go r.run(runCtx)
	return runID, nil
}

// run executes stages sequentially, threading each stage's output into the next
// via ${input.*}, and short-circuits the pipeline on the first stage failure.
func (r *PipelineRunner) run(ctx context.Context) {
	e := r.engine
	if r.spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.spec.Timeout)
		defer cancel()
	}

	var upstream task.InputContext
	var lastReturn interface{}
	for i, st := range r.spec.Stages {
		isTerminal := i == len(r.spec.Stages)-1

		// Terminal daemon stage: the pipeline's lifetime is the daemon's
		// lifetime. Fire the daemon stage WITHOUT the wait-to-success gate,
		// leave the pipeline 'running', then block on the daemon run's
		// terminal state and finish the pipeline with the daemon's *actual*
		// status (not a coerced failure). Daemon-ness is decided from the
		// merged dispatch spec inside fireStageRaw — a stage trigger override
		// could flip daemon either way.
		if isTerminal {
			runID, isDaemon, err := e.fireStageRaw(ctx, st, upstream, r.runID)
			if err != nil {
				reason := fmt.Sprintf("stage %d (%s): %s", i, st.Task, err.Error())
				e.log.Warn("pipeline terminal stage dispatch failed; pipeline failed",
					zap.String("pipeline", r.spec.ID), zap.Int("stage", i),
					zap.String("task", st.Task), zap.Error(err))
				r.finish(registry.StatusFailure, reason, nil)
				return
			}
			if isDaemon {
				r.runTerminalDaemon(i, st, upstream, runID)
				return
			}
			// Terminal non-daemon stage: wait to terminal as usual.
			out, werr := e.awaitStageSuccess(ctx, runID)
			if werr != nil {
				reason := fmt.Sprintf("stage %d (%s): %s", i, st.Task, werr.Error())
				e.log.Warn("pipeline stage failed; pipeline failed",
					zap.String("pipeline", r.spec.ID), zap.Int("stage", i),
					zap.String("task", st.Task), zap.Error(werr))
				r.finish(registry.StatusFailure, reason, nil)
				return
			}
			r.finish(registry.StatusSuccess, "", out.Output)
			return
		}

		out, err := e.dispatchStage(ctx, st, upstream, r.runID)
		if err != nil {
			reason := fmt.Sprintf("stage %d (%s): %s", i, st.Task, err.Error())
			e.log.Warn("pipeline stage failed; pipeline failed",
				zap.String("pipeline", r.spec.ID), zap.Int("stage", i),
				zap.String("task", st.Task), zap.Error(err))
			r.finish(registry.StatusFailure, reason, nil)
			return
		}
		upstream = out
		lastReturn = out.Output
	}
	// Reachable only for an empty-stages pipeline (Validate rejects that), but
	// keep the success terminal for completeness.
	r.finish(registry.StatusSuccess, "", lastReturn)
}

// runTerminalDaemon ties the pipeline's lifetime to a daemon terminal stage's
// run. It registers the pipeline as "live", records the daemon run ID + the
// upstream context that fed the terminal stage, then blocks on the daemon's
// terminal state and finishes the pipeline with the daemon's *actual* status.
//
// A KillRun on the pipeline parent (which cancels runCtx) is propagated to the
// daemon stage run by the runCtx watcher below — the daemon's own run then
// transitions to its cancelled terminal, which becomes the pipeline's.
//
// stageIdx/stage/upstream are recorded for the mid-pipeline re-fire path added
// in PR4 Task 19.
func (r *PipelineRunner) runTerminalDaemon(stageIdx int, stage task.Stage, upstream task.InputContext, daemonRunID string) {
	e := r.engine
	r.mu.Lock()
	r.terminalIdx = stageIdx
	r.terminalStage = stage
	r.terminalUpstream = upstream
	r.daemonRunID = daemonRunID
	r.mu.Unlock()

	e.registerLivePipeline(r)
	defer e.unregisterLivePipeline(r.spec.ID, r)

	// Propagate a pipeline-parent KillRun (runCtx cancellation) into the
	// terminal daemon run so the daemon's own run reaches its cancelled
	// terminal and that status flows up. Stop the watcher when we return.
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-r.runCtx.Done():
			e.KillRun(daemonRunID)
		case <-watchDone:
		}
	}()

	// Block on the daemon run's terminal state with a background context so the
	// daemon's lifetime — not the trigger call's context — governs when the
	// pipeline finishes. Finish the pipeline with the daemon's *actual* status.
	res, werr := e.WaitRun(context.Background(), daemonRunID)
	status := res.Status
	ret := res.ReturnValue
	if werr != nil {
		// WaitRun with a background context only errors on a pathological
		// internal failure (never ctx cancellation). Surface it loudly and
		// stop the orphaned daemon subprocess.
		e.log.Error("pipeline terminal daemon wait failed",
			zap.String("pipeline", r.spec.ID),
			zap.String("daemon_run", daemonRunID),
			zap.Error(werr))
		e.KillRun(daemonRunID)
		status = registry.StatusFailure
		ret = nil
	}
	r.finish(status, "", ret)
}

// fireStageRaw resolves the stage's overrides + ${input.*} references against
// the upstream stage's output and fires the stage as a kind: Task child of the
// pipeline run, returning the stage's run ID WITHOUT waiting for it to reach a
// terminal state. It also reports whether the *merged* dispatch spec is a
// daemon — a stage trigger override could flip daemon either way, so daemon-ness
// is decided after applying overrides, not from the bare registry spec.
//
// dispatchStage = fireStageRaw + awaitStageSuccess; the terminal-daemon path in
// run() uses fireStageRaw directly so it can leave the pipeline 'running' and
// track the daemon run instead of gating on success.
func (e *Engine) fireStageRaw(ctx context.Context, st task.Stage, upstream task.InputContext, parentRunID string) (string, bool, error) {
	ref, ok := e.registry.Get(st.Task) // Get filters to kind: Task — stages are always Task
	if !ok {
		return "", false, fmt.Errorf("stage task %q not registered", st.Task)
	}

	dispatchSpec := ref
	if st.Overrides != nil {
		ovPtr := st.Overrides
		if st.Overrides.Params != nil {
			resolved, rerr := task.ResolveInputOutputList(st.Overrides.Params, upstream)
			if rerr != nil {
				return "", false, fmt.Errorf("resolve input refs: %w", rerr)
			}
			ovCopy := *st.Overrides
			ovCopy.Params = resolved
			ovPtr = &ovCopy
		}
		merged := taskset.ApplyOverrides(ref, ovPtr)
		if vErr := merged.Validate(); vErr != nil {
			return "", false, fmt.Errorf("overrides produce invalid spec: %w", vErr)
		}
		dispatchSpec = merged
	}

	runID, err := e.fireAsync(ctx, dispatchSpec, pkgruntime.RunOptions{ParentRunID: parentRunID}, registry.TriggerPipelineStage)
	if err != nil {
		return "", false, fmt.Errorf("dispatch: %w", err)
	}
	return runID, dispatchSpec.Trigger.Daemon, nil
}

// awaitStageSuccess blocks until the stage run reaches a terminal state and
// returns its InputContext for the next stage. A non-success terminal (or a ctx
// cancellation) is an error; on ctx cancellation it kills the orphaned stage
// subprocess rather than leaving it running detached.
func (e *Engine) awaitStageSuccess(ctx context.Context, runID string) (task.InputContext, error) {
	res, werr := e.WaitRun(ctx, runID)
	if werr != nil {
		// ctx cancelled (pipeline timeout or KillRun on the parent) — stop the
		// orphaned stage subprocess rather than leaving it running detached.
		e.KillRun(runID)
		return task.InputContext{}, fmt.Errorf("wait: %w", werr)
	}
	if res.Status != registry.StatusSuccess {
		return task.InputContext{}, fmt.Errorf("run %s ended with status %s", runID, res.Status)
	}
	// Only Output is threaded between stages in v1: PipelineTask.Validate rejects
	// ${input.params.*} refs, so there is deliberately no Params snapshot here.
	// Cross-stage param threading is a planned follow-up (mirrors the forward-compat
	// note on the preflight dispatchPipelineStage path).
	return task.InputContext{Output: res.ReturnValue}, nil
}

// dispatchStage resolves + fires a stage and blocks until it reaches a terminal
// state, returning the stage's InputContext for the next stage. Used for
// non-terminal stages (and as the success-gated composition of fireStageRaw +
// awaitStageSuccess).
func (e *Engine) dispatchStage(ctx context.Context, st task.Stage, upstream task.InputContext, parentRunID string) (task.InputContext, error) {
	runID, _, err := e.fireStageRaw(ctx, st, upstream, parentRunID)
	if err != nil {
		return task.InputContext{}, err
	}
	return e.awaitStageSuccess(ctx, runID)
}

// finish marks the pipeline's parent run terminal and records the terminal
// stage's return value as the pipeline's own return (persisted to the run row,
// so chain consumers and WaitRun observe it — mirrors how runTask persists).
func (r *PipelineRunner) finish(status, reason string, ret interface{}) {
	e := r.engine
	// Mark finished under mu so an in-flight handlePipelineStageRerun that has
	// already passed its liveness check bails before touching a torn-down runner.
	r.mu.Lock()
	r.finished = true
	r.mu.Unlock()
	finishCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if ret != nil {
		if b, err := json.Marshal(ret); err == nil {
			_ = e.registry.SetRunResult(finishCtx, r.runID, string(b), "", "")
		}
	}
	if reason != "" {
		_ = e.registry.FinishRunWithReason(finishCtx, r.runID, status, reason)
	} else {
		_ = e.registry.FinishRun(finishCtx, r.runID, status)
	}
	// Tear down the run-lifecycle registrations now that the DB row is terminal.
	// Close runDone AFTER the DB write so a WaitRun goroutine woken by the close
	// reads the finalized row.
	if v, ok := e.runDone.LoadAndDelete(r.runID); ok {
		close(v.(chan struct{}))
	}
	// Pipeline-as-chain-source: fire downstream subscribers off the pipeline's
	// overall outcome (fresh background context — finishCtx is about to expire).
	// Done before dropping runTriggerSource, which FireChain's failure-path
	// guards consult.
	e.FireChain(context.Background(), r.spec.ID, r.runID, status, ret, nil)
	e.runCancels.Delete(r.runID)
	e.runTriggerSource.Delete(r.runID)
	if r.cancel != nil {
		r.cancel()
	}
}

// registerLivePipeline records a runner whose terminal daemon stage is up.
// Keyed by pipeline ID; the latest fire wins (a pipeline only has one live
// daemon-terminal fire at a time). The mid-pipeline re-fire path (PR4 Task 19)
// consumes this set.
func (e *Engine) registerLivePipeline(r *PipelineRunner) {
	e.livePipelineMu.Lock()
	e.livePipelines[r.spec.ID] = r
	e.livePipelineMu.Unlock()
}

// unregisterLivePipeline removes a runner from the live set, but only if the
// stored runner is still this one — a newer fire of the same pipeline ID may
// have replaced it, and we must not clobber the newer registration when an old
// runner's terminal daemon finally exits.
func (e *Engine) unregisterLivePipeline(pipelineID string, r *PipelineRunner) {
	e.livePipelineMu.Lock()
	if e.livePipelines[pipelineID] == r {
		delete(e.livePipelines, pipelineID)
	}
	e.livePipelineMu.Unlock()
}
