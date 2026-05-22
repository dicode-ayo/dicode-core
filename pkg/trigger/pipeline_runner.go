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
// daemon's lifetime: it registers itself in Engine.livePipelines (so a
// mid-pipeline stage re-fire — handlePipelineStageRerun — can find it) and
// records the terminal stage's position, the upstream context that fed it, and
// the current daemon run ID under mu. Those fields are read+written from both
// the runner goroutine and the re-fire goroutine, hence the mutex.
type PipelineRunner struct {
	engine *Engine
	spec   *task.PipelineTask
	runID  string             // the pipeline's own parent run ID
	cancel context.CancelFunc // cancels runCtx; invoked on finish + by KillRun
	runCtx context.Context    // the pipeline's lifecycle context (cancelled by KillRun/finish)

	mu sync.Mutex
	// live-daemon-terminal-stage state, set once the terminal daemon stage is
	// fired and read by handlePipelineStageRerun. All guarded by mu.
	terminalIdx      int               // index of the terminal stage in spec.Stages
	terminalStage    task.Stage        // the terminal daemon stage (re-fired on restart)
	terminalUpstream task.InputContext // the upstream ${input} that fed the terminal stage
	daemonRunID      string            // current terminal daemon stage run ID
	finished         bool              // set by finish(); blocks late re-fires

	// restart coordination between the terminal-daemon wait loop
	// (runTerminalDaemon) and a mid-pipeline re-fire (propagatePipelineStageRerun).
	// When a re-fire begins it sets restarting=true (under mu) BEFORE killing the
	// current daemon run, so when the wait loop's WaitRun returns it knows the
	// terminal was a deliberate restart (not a real exit) and blocks on
	// restartDone until the new daemon run is published (or the restart aborts).
	restarting  bool
	restartDone chan struct{} // closed by the re-fire when it publishes the new run / aborts
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
// run. It registers the pipeline as "live" (so a mid-pipeline stage re-fire can
// find it via handlePipelineStageRerun), records the daemon run ID + the
// upstream context that fed the terminal stage (needed to replay descendants and
// restart on re-fire), then blocks on the daemon's terminal state and finishes
// the pipeline with the daemon's *actual* status.
//
// A KillRun on the pipeline parent (which cancels runCtx) is propagated to the
// daemon stage run by waitDaemonRun's runCtx watcher — the daemon's own run
// then transitions to its cancelled terminal, which becomes the pipeline's.
//
// stageIdx/stage are recorded so handlePipelineStageRerun knows where the
// terminal stage sits and how to re-fire it.
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
	// CURRENT terminal daemon run. The watcher re-reads daemonRunID under mu so
	// it kills whichever run is live after a mid-pipeline restart swap. Stop it
	// when the loop exits.
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-r.runCtx.Done():
			r.mu.Lock()
			cur := r.daemonRunID
			r.mu.Unlock()
			e.KillRun(cur)
		case <-watchDone:
		}
	}()

	// Loop: wait on the current daemon run. When it terminates, either it was a
	// deliberate mid-pipeline restart (restarting==true → wait for the new run
	// and loop) or a real exit (→ finish the pipeline with the daemon's status).
	for {
		r.mu.Lock()
		current := r.daemonRunID
		r.mu.Unlock()

		res, werr := e.WaitRun(context.Background(), current)
		status := res.Status
		ret := res.ReturnValue
		if werr != nil {
			// With a background context WaitRun never errors on cancellation; it
			// errors only on a DB read failure or a missing run row
			// (ErrRunNotFound). Either way the run's terminal status is
			// unrecoverable here — surface it loudly and stop the (possibly still
			// live) daemon subprocess rather than leaving it orphaned.
			e.log.Error("pipeline terminal daemon wait failed",
				zap.String("pipeline", r.spec.ID),
				zap.String("daemon_run", current),
				zap.Error(werr))
			e.KillRun(current)
			status = registry.StatusFailure
			ret = nil
		}

		// Decide whether this terminal was a deliberate mid-pipeline restart.
		// The checks must be ordered to be robust regardless of how far the
		// re-fire's handshake has progressed by the time we wake:
		//
		//  1. daemonRunID already advanced past `current` → the re-fire has
		//     published the new run (and may have already cleared restarting);
		//     loop and wait on the new run.
		//  2. restarting==true but daemonRunID still == current → a re-fire is
		//     in flight but hasn't published yet; block on restartDone, then
		//     re-decide (it either published a new run or aborted).
		//  3. otherwise → a real exit; finish the pipeline with this status.
		//
		// Checking daemonRunID before restarting closes the race where the
		// re-fire completes the whole handshake (publish + clear restarting +
		// close restartDone) before this goroutine is scheduled.
		r.mu.Lock()
		if r.daemonRunID != current {
			r.mu.Unlock()
			continue
		}
		if r.restarting {
			done := r.restartDone
			r.mu.Unlock()
			<-done // closed by propagatePipelineStageRerun when the new run is published / restart aborted
			r.mu.Lock()
			if r.daemonRunID != current {
				// Restart published a new run; wait on it.
				r.mu.Unlock()
				continue
			}
			// Restart aborted (descendant failed / shutting down): daemonRunID
			// still points at the killed run. Finish with its terminal status.
			r.mu.Unlock()
			r.finish(status, "", ret)
			return
		}
		r.mu.Unlock()

		r.finish(status, "", ret)
		return
	}
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

	// INVARIANT: the registry.TriggerPipelineStage source here is load-bearing,
	// not cosmetic. runTask keys off it (engine.go:onDaemonRunFinished gate) to
	// skip the standalone-daemon onDaemonRunFinished lifecycle hook for pipeline
	// stages — pipeline-owned daemon runs must not flip global DaemonState or
	// schedule restarts (#344). Any future stage-dispatch path MUST fire with
	// this source, or daemon-lifecycle suppression silently stops protecting.
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

// registerLivePipeline records a runner whose terminal daemon stage is up, so
// handlePipelineStageRerun can find it. Keyed by pipeline ID; the latest fire
// wins (restart coalescing via restartGates ensures at most one in-flight
// re-fire per pipeline, and a pipeline only has one live daemon-terminal fire
// at a time).
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

// pipelineRestartGateKey namespaces a pipeline ID for use as a restartGates
// key. restartGates is shared with the standalone-daemon restart path, which
// keys it by daemon task ID; the prefix prevents a pipeline whose ID equals a
// daemon task ID from cross-coalescing with that daemon's gate (#344).
func pipelineRestartGateKey(pipelineID string) string {
	return "pipeline:" + pipelineID
}

// pipelineContainsStage reports whether the pipeline lists taskID as a stage.
func pipelineContainsStage(p *task.PipelineTask, taskID string) bool {
	for _, st := range p.Stages {
		if st.Task == taskID {
			return true
		}
	}
	return false
}

// handlePipelineStageRerun is the post-success hook the engine calls from
// FireChain when ANY kind: Task finishes with status=success. It is the
// PipelineTask analogue of notifyPrereqCompletion → propagateBeforeRerun.
//
// For each registered kind: PipelineTask that (a) lists completedTaskID as a
// stage and (b) is currently *live* (terminal daemon stage running, tracked in
// livePipelines), it replays the stages AFTER the completed one with fresh
// ${input} (seeded from the re-fired stage's output) and then restarts the
// terminal daemon so it picks up the freshly-rendered descendants.
//
// Coalescing + shutdown/unregister handling mirror propagateBeforeRerun exactly:
// restartGates.tryAcquire (keyed by pipeline ID) → defer release → bail on
// shutdown / not-live → descendant loop with per-iteration re-checks → kill+wait
// the old daemon run → re-fire the terminal daemon stage.
func (e *Engine) handlePipelineStageRerun(completedTaskID string, output interface{}) {
	for _, k := range e.registry.AllKinded() {
		p, ok := k.(*task.PipelineTask)
		if !ok {
			continue
		}
		if !pipelineContainsStage(p, completedTaskID) {
			continue
		}
		// Only live pipelines (terminal daemon stage running) are eligible.
		// A re-fire of a stage in a pipeline that isn't currently running its
		// terminal daemon has nothing to propagate to.
		e.livePipelineMu.Lock()
		runner := e.livePipelines[p.ID]
		e.livePipelineMu.Unlock()
		if runner == nil {
			continue
		}
		e.propagatePipelineStageRerun(runner, completedTaskID, output)
	}
}

// propagatePipelineStageRerun replays the descendants of a re-fired pipeline
// stage and restarts the terminal daemon. Structurally mirrors
// propagateBeforeRerun (daemon_state.go): coalesce via restartGates, bail on
// shutdown / the pipeline no longer being live, replay descendants with fresh
// ${input}, then kill+wait the old daemon run and re-fire the terminal stage.
func (e *Engine) propagatePipelineStageRerun(runner *PipelineRunner, reranTaskID string, reranReturn interface{}) {
	pipelineID := runner.spec.ID
	// restartGates is shared with the standalone-daemon restart path
	// (propagateBeforeRerun), which keys it by daemon TASK ID. Namespace the
	// pipeline key so a pipeline whose ID happens to equal a daemon task ID
	// can't cross-coalesce with that daemon's restart gate (#344, LOW).
	gateKey := pipelineRestartGateKey(pipelineID)
	if !e.restartGates.tryAcquire(gateKey) {
		// A propagation/restart is already queued or in flight; coalesce.
		e.log.Debug("pipeline stage re-fire coalesced", zap.String("pipeline", pipelineID))
		return
	}
	go func() {
		defer e.restartGates.release(gateKey)

		// Find the re-fired stage's index in the pipeline's stages.
		startIdx := -1
		for i, st := range runner.spec.Stages {
			if st.Task == reranTaskID {
				startIdx = i
				break
			}
		}
		if startIdx < 0 {
			// Defensive: handlePipelineStageRerun already verified membership.
			return
		}

		// Bail if the engine is shutting down or the pipeline is no longer
		// live (its terminal daemon already exited / the pipeline finished)
		// between the liveness check and this goroutine being scheduled.
		// Mirrors propagateBeforeRerun's canonical guard.
		if e.isShuttingDown() {
			e.log.Debug("pipeline stage re-fire skipped: engine shutting down",
				zap.String("pipeline", pipelineID))
			return
		}
		if !e.pipelineStillLive(pipelineID, runner) {
			e.log.Debug("pipeline stage re-fire skipped: pipeline no longer live",
				zap.String("pipeline", pipelineID))
			return
		}

		runner.mu.Lock()
		terminalIdx := runner.terminalIdx
		terminalStage := runner.terminalStage
		oldDaemonRunID := runner.daemonRunID
		storedUpstream := runner.terminalUpstream
		runner.mu.Unlock()

		e.log.Info("pipeline mid-stage re-fire: propagating to descendants",
			zap.String("pipeline", pipelineID),
			zap.String("rerun", reranTaskID),
			zap.Int("stage", startIdx),
			// Non-terminal descendants replayed below [startIdx+1 .. terminalIdx-1];
			// the terminal daemon stage is restarted separately.
			zap.Int("replayed_descendants", terminalIdx-startIdx-1),
		)

		// Replay the NON-terminal descendants [startIdx+1 .. terminalIdx-1]
		// with fresh ${input}, seeded from the re-fired stage's return value.
		// The terminal stage itself is restarted separately below (it's the
		// daemon). Use the engine's shutdown-cancellable context so an
		// in-flight stage bails on Shutdown; the between-iteration guards only
		// catch the gaps, not a stage already inside Execute.
		stageCtx := e.getShutdownCtx()
		if stageCtx == nil {
			stageCtx = context.Background()
		}
		// upstream is what feeds the FIRST replayed descendant. When the
		// re-fired stage is the terminal daemon stage itself (startIdx ==
		// terminalIdx — an operator restarting the daemon's own task), there
		// are no descendants to replay and the terminal stage keeps the
		// upstream that originally fed it; re-seeding it with the daemon's own
		// return would be wrong. Otherwise the first descendant is fed by the
		// re-fired stage's fresh return value.
		upstream := task.InputContext{Output: reranReturn}
		if startIdx >= terminalIdx {
			upstream = storedUpstream
		}
		for i := startIdx + 1; i < terminalIdx; i++ {
			if e.isShuttingDown() {
				e.log.Debug("pipeline stage re-fire aborted: engine shutting down",
					zap.String("pipeline", pipelineID), zap.Int("stage", i))
				return
			}
			if !e.pipelineStillLive(pipelineID, runner) {
				e.log.Debug("pipeline stage re-fire aborted: pipeline no longer live",
					zap.String("pipeline", pipelineID), zap.Int("stage", i))
				return
			}
			out, err := e.dispatchStage(stageCtx, runner.spec.Stages[i], upstream, runner.runID)
			if err != nil {
				e.log.Warn("pipeline mid-stage re-fire: descendant stage failed; pipeline left on old daemon",
					zap.String("pipeline", pipelineID), zap.Int("stage", i), zap.Error(err))
				return
			}
			upstream = out
		}

		// Last guard before restarting the daemon: the pipeline could have
		// finished (terminal daemon exited) or the engine started shutting
		// down while we replayed descendants.
		if e.isShuttingDown() {
			e.log.Debug("pipeline stage re-fire restart skipped: engine shutting down",
				zap.String("pipeline", pipelineID))
			return
		}
		if !e.pipelineStillLive(pipelineID, runner) {
			e.log.Debug("pipeline stage re-fire restart skipped: pipeline no longer live",
				zap.String("pipeline", pipelineID))
			return
		}

		// Enter the restart handshake. Setting restarting=true + a fresh
		// restartDone BEFORE killing the old run tells the runner's wait loop
		// that the imminent terminal of oldDaemonRunID is a deliberate restart,
		// not a real exit — so it blocks on restartDone instead of finishing
		// the pipeline. We MUST resolve the handshake (publish the new run or
		// clear restarting) on every exit path from here, or the wait loop
		// blocks forever; the deferred cleanup below guarantees that.
		restartDone := make(chan struct{})
		runner.mu.Lock()
		runner.restarting = true
		runner.restartDone = restartDone
		runner.mu.Unlock()

		published := false
		defer func() {
			// If we exit before publishing the new run (descendant of the
			// restart phase failed), leave daemonRunID pointing at the old
			// (now-killed) run and clear restarting; the wait loop detects the
			// unchanged run ID and finishes the pipeline with the killed
			// daemon's status. Closing restartDone unblocks it.
			if !published {
				runner.mu.Lock()
				runner.restarting = false
				runner.mu.Unlock()
			}
			close(restartDone)
		}()

		// Kill the current terminal daemon run and wait for it to drain.
		// Mirrors propagateBeforeRerun: KillRun(old) + bounded WaitRun(old).
		if oldDaemonRunID != "" {
			e.KillRun(oldDaemonRunID)
			waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = e.WaitRun(waitCtx, oldDaemonRunID)
			cancel()
		}

		// Re-fire the terminal daemon stage with the freshly-replayed upstream.
		newRunID, isDaemon, ferr := e.fireStageRaw(stageCtx, terminalStage, upstream, runner.runID)
		if ferr != nil {
			e.log.Error("pipeline mid-stage re-fire: terminal daemon restart failed",
				zap.String("pipeline", pipelineID), zap.Error(ferr))
			return
		}
		if !isDaemon {
			// Defensive: the terminal stage was a daemon when we registered as
			// live; a registry mutation could in principle flip it. Stop the
			// stray run rather than leaving it untracked, and let the wait loop
			// finish the pipeline on the old (killed) daemon's status.
			e.log.Warn("pipeline mid-stage re-fire: terminal stage no longer a daemon after restart; stopping stray run",
				zap.String("pipeline", pipelineID))
			e.KillRun(newRunID)
			return
		}

		// Publish the new daemon run + upstream so the runner's wait loop
		// re-waits on it. Clear restarting under the same lock so the loop sees
		// a consistent snapshot.
		//
		// Two abort conditions, checked under the SAME mutex as the publish so
		// there's no TOCTOU between "is the pipeline still alive?" and "adopt the
		// fresh run":
		//
		//   - runner.finished: the pipeline already finished (its terminal-daemon
		//     wait loop ran finish()) while we were restarting.
		//   - runner.runCtx.Err() != nil: the pipeline parent was KillRun'd /
		//     cancelled while we were restarting. The single-shot runCtx watcher
		//     in runTerminalDaemon may have already fired on the now-dead OLD run
		//     (it's spent), so adopting the fresh run here would leave it with
		//     nothing to kill it → leaked daemon subprocess + pipeline wedged
		//     'running' (#344, HIGH). Refuse to adopt: kill the fresh daemon and
		//     leave daemonRunID pointing at the killed old run so the wait loop's
		//     "restart aborted" branch finishes the pipeline with the old run's
		//     terminal status (cancelled).
		//
		// The deferred cleanup (published==false) clears restarting + closes
		// restartDone on either abort, unblocking the wait loop.
		runner.mu.Lock()
		if runner.finished {
			runner.mu.Unlock()
			e.log.Debug("pipeline stage re-fire: pipeline finished during restart; stopping fresh daemon run",
				zap.String("pipeline", pipelineID))
			e.KillRun(newRunID)
			return
		}
		if runner.runCtx.Err() != nil {
			runner.mu.Unlock()
			e.log.Debug("pipeline stage re-fire: pipeline cancelled during restart; stopping fresh daemon run",
				zap.String("pipeline", pipelineID))
			e.KillRun(newRunID)
			return
		}
		runner.daemonRunID = newRunID
		runner.terminalUpstream = upstream
		runner.restarting = false
		runner.mu.Unlock()
		published = true
	}()
}

// pipelineStillLive reports whether the given runner is still the live
// registration for pipelineID AND has not yet finished. Used as the
// propagation-loop guard (analogue of daemonRegistered for the daemon path).
func (e *Engine) pipelineStillLive(pipelineID string, r *PipelineRunner) bool {
	e.livePipelineMu.Lock()
	cur := e.livePipelines[pipelineID]
	e.livePipelineMu.Unlock()
	if cur != r {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.finished
}
