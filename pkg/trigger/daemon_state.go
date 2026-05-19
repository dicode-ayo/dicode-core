package trigger

import (
	"context"
	"sync"
	"time"

	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// beforeContains reports whether any BeforeEntry in entries names taskID.
// Pulls only the Task field; per-edge overrides are irrelevant for
// membership checks.
func beforeContains(entries []task.BeforeEntry, taskID string) bool {
	for _, e := range entries {
		if e.Task == taskID {
			return true
		}
	}
	return false
}

// DaemonState describes the preflight/lifecycle phase of a daemon task as
// observed by the trigger engine. Surfaced via Engine.DaemonState so the
// WebUI (and operators reading API responses) can see *why* a daemon isn't
// up — distinguishing "still waiting on the render task" from "render
// failed" from "explicitly stopped".
//
// The seven values are intentionally a closed set rather than a bag of
// booleans. Adding a new phase should require an explicit case in any
// switch that consumes this type.
type DaemonState string

const (
	// DaemonStopped is the zero value. Returned for any task ID the engine
	// hasn't tracked yet — including non-daemon tasks and unknown IDs — so
	// callers don't have to special-case "not found". Also the resting
	// state after a deliberate Unregister or engine shutdown, AND after a
	// daemon body exits cleanly (status=success) with no configured
	// auto-restart. See DaemonCrashed for the non-success counterpart.
	DaemonStopped DaemonState = "stopped"

	// DaemonPrereqRunning indicates the engine has dispatched the daemon's
	// trigger.before tasks and is waiting for them to complete.
	DaemonPrereqRunning DaemonState = "prereq_running"

	// DaemonPrereqFailed indicates at least one prereq finished with
	// status=failure (or was cancelled). The daemon was NOT started; the
	// engine surfaces the most recent prereq run for the operator to inspect.
	DaemonPrereqFailed DaemonState = "prereq_failed"

	// DaemonRunning indicates the daemon's container/process is up. Set
	// after preflight succeeds (or immediately, when before: is empty).
	DaemonRunning DaemonState = "running"

	// DaemonStopping indicates a restart is in flight — the engine is
	// tearing down the current run before re-firing preflight. Distinct
	// from DaemonStopped so operators can see the brief intermediate state
	// during a prereq-driven restart.
	DaemonStopping DaemonState = "stopping"

	// DaemonFailedAfterPreflight indicates the daemon's preflight pipeline
	// completed (or was deliberately skipped via skipPrereqs=true from the
	// mid-pipeline re-fire path) but the subsequent fireAsync call to
	// launch the daemon body failed — e.g. binary missing, port already
	// bound, runtime resource exhaustion. Distinct from DaemonStopped so
	// operators can tell "preflight is fine, daemon body broke" apart from
	// "operator deliberately stopped it / never started it". Reachable
	// only from startDaemonInternal's post-preflight error branch (issue
	// #318).
	DaemonFailedAfterPreflight DaemonState = "failed_after_preflight"

	// DaemonCrashed indicates the daemon's body successfully started but
	// then exited with any non-success status (failure, cancelled, etc.)
	// AND the configured restart policy will NOT restart it — i.e. either
	// restart=never, or restart=on-failure with a status the engine
	// doesn't treat as a failure. Reachable only from
	// onDaemonRunFinished's no-restart branch.
	//
	// Distinct from DaemonStopped (clean exit / operator-initiated) and
	// from DaemonFailedAfterPreflight (fireAsync-time error, before the
	// daemon body got to run) so operators can tell the three apart in
	// the WebUI / API.
	//
	// Terminal: like DaemonFailedAfterPreflight, an operator must re-fire
	// the daemon (e.g. via manual run or a registry reload) to retry. The
	// engine does not automatically transition out of this state.
	DaemonCrashed DaemonState = "crashed"
)

// daemonStateMap is a thread-safe taskID → DaemonState map. Kept as a
// dedicated type so we can swap in a fancier representation later (e.g.
// per-state event hooks) without touching every call site.
type daemonStateMap struct {
	mu sync.RWMutex
	m  map[string]DaemonState
}

func newDaemonStateMap() *daemonStateMap {
	return &daemonStateMap{m: make(map[string]DaemonState)}
}

func (d *daemonStateMap) get(taskID string) DaemonState {
	d.mu.RLock()
	defer d.mu.RUnlock()
	s, ok := d.m[taskID]
	if !ok {
		return DaemonStopped
	}
	return s
}

func (d *daemonStateMap) set(taskID string, s DaemonState) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s == DaemonStopped {
		// Conserve memory: stopped is the default, no need to persist it.
		delete(d.m, taskID)
		return
	}
	d.m[taskID] = s
}

// DaemonState returns the current lifecycle phase of the daemon with the
// given task ID. Returns DaemonStopped for unknown / never-started tasks.
//
// Safe for concurrent use.
func (e *Engine) DaemonState(taskID string) DaemonState {
	return e.daemonStates.get(taskID)
}

// setDaemonState records the daemon's lifecycle phase. Internal helper:
// only the engine's daemon-management paths should call it. Setting state
// to DaemonStopped removes the entry from the map.
func (e *Engine) setDaemonState(taskID string, s DaemonState) {
	e.daemonStates.set(taskID, s)
}

// daemonRegistered reports whether taskID is currently tracked in
// daemonSpecs — i.e. the engine still considers this task a live daemon.
// Used by the propagation goroutine to bail out if Unregister has run
// between the time the goroutine was launched and the time it gets
// scheduled (or between successive descendant dispatches in a long
// pipeline). Mirrors the stillRegistered check in onDaemonRunFinished.
func (e *Engine) daemonRegistered(taskID string) bool {
	e.daemonMu.Lock()
	_, ok := e.daemonSpecs[taskID]
	e.daemonMu.Unlock()
	return ok
}

// notifyPrereqCompletion is the post-success hook the engine calls from
// FireChain when ANY task finishes with status=success. It walks the
// registered daemons and, for each one that lists completedTaskID in
// its trigger.before, propagates the re-fire to descendant stages and
// then restarts the daemon.
//
// completedReturn is the upstream's return value, passed through as
// the initial ${input.output} for the descendant stages. When the
// upstream's run_result is a non-string (or nil), descendants whose
// overrides reference ${input.output} will fail loudly via
// ErrInputUnavailable — matches the FireChain semantics from PR #299.
//
// Coalescing: at most one propagation per daemon is in flight at any
// time — concurrent prereq completions are dropped via
// restartGate.tryAcquire. This prevents thrash when a daemon depends
// on a prereq that's on a short cron (credential rotation, periodic
// config render, etc.).
func (e *Engine) notifyPrereqCompletion(completedTaskID string, completedReturn interface{}) {
	for _, spec := range e.registry.All() {
		if !spec.Trigger.Daemon {
			continue
		}
		if !beforeContains(spec.Trigger.Before, completedTaskID) {
			continue
		}
		// Only attempt a propagation if the daemon was actually up.
		// Re-firing descendants for a daemon we never managed to start
		// (e.g. PrereqFailed on first boot) would race with the
		// in-flight first-boot path; the prereq's success will fall
		// through to the normal startDaemon path via the next
		// registration cycle.
		state := e.DaemonState(spec.ID)
		if state != DaemonRunning {
			e.log.Debug("prereq completion ignored — daemon not Running",
				zap.String("daemon", spec.ID),
				zap.String("prereq", completedTaskID),
				zap.String("state", string(state)),
			)
			continue
		}
		e.propagateBeforeRerun(spec, completedTaskID, completedReturn)
	}
}

// propagateBeforeRerun replays the descendants of a re-fired preflight
// stage. When task X re-runs successfully and X is at index i of
// daemon D's trigger.before pipeline, re-fire stages [i+1..n-1]
// sequentially with X's fresh return value as the initial
// upstreamOutput. After the last descendant succeeds, restart the
// daemon (without re-running preflight — descendants are already
// fresh) so it picks up the newly-rendered config.
//
// Stages [0..i-1] are NOT re-fired — they remain at their last
// successful run. Only descendants flow through.
//
// If any descendant fails during propagation, the daemon stays at
// its current state (running on old config). Matches PR #300's
// failure semantics.
//
// Coalesced via restartGate so a flurry of re-fires produces at most
// one outstanding propagation per daemon.
func (e *Engine) propagateBeforeRerun(daemonSpec *task.Spec, reranTaskID string, reranReturn interface{}) {
	if !e.restartGates.tryAcquire(daemonSpec.ID) {
		// A propagation/restart is already queued or in flight; coalesce.
		e.log.Debug("daemon restart coalesced", zap.String("task", daemonSpec.ID))
		return
	}
	go func() {
		defer e.restartGates.release(daemonSpec.ID)

		// Wrap the re-fired stage's return value in an InputContext.
		// The resolver type-asserts per-token at dispatch time:
		// ${input.output} requires a string, ${input.output.X} a map,
		// etc. — so a non-string reranReturn yields ErrInputUnavailable
		// loudly rather than silently passing a literal token to
		// descendants. Params is nil here because preflight stages run
		// with empty RunOptions.Params today.
		initialCtx := task.InputContext{Output: reranReturn}

		// Find the re-fired stage's index in the daemon's before list.
		startIdx := -1
		for i, entry := range daemonSpec.Trigger.Before {
			if entry.Task == reranTaskID {
				startIdx = i
				break
			}
		}
		if startIdx < 0 {
			// Defensive: notifyPrereqCompletion already verified
			// membership via beforeContains, so this shouldn't fire.
			// Bail without restarting rather than do something
			// surprising on a registry mutation race.
			return
		}

		// Bail early if the engine is shutting down or the daemon was
		// unregistered between notifyPrereqCompletion's check and our
		// goroutine being scheduled. Matches the canonical guard from
		// onDaemonRunFinished — without it, the propagation loop would
		// happily dispatch descendants and re-fire startDaemonInternal
		// on a torn-down daemon, leaking daemonRuns entries and racing
		// the shutdown path.
		if e.isShuttingDown() {
			e.log.Debug("propagation skipped: engine shutting down",
				zap.String("daemon", daemonSpec.ID))
			return
		}
		if !e.daemonRegistered(daemonSpec.ID) {
			e.log.Debug("propagation skipped: daemon unregistered",
				zap.String("daemon", daemonSpec.ID))
			return
		}

		e.log.Info("daemon mid-pipeline re-fire: propagating to descendants",
			zap.String("task", daemonSpec.ID),
			zap.String("rerun", reranTaskID),
			zap.Int("stage", startIdx),
			zap.Int("descendants", len(daemonSpec.Trigger.Before)-startIdx-1),
		)

		// Replay descendants sequentially with fresh ${input.output}.
		// Re-check the shutdown/unregister guard at the top of each
		// iteration: a long pipeline (many stages, each potentially
		// dispatching a real container) can easily span a shutdown or
		// unregister window opened by an operator while we're mid-loop.
		//
		// Use the engine's shutdown-cancellable context for each stage
		// dispatch so an in-flight fireAsync / WaitRun bails when
		// Shutdown is called — the between-iteration guards above only
		// catch the gaps, not a stage already inside Execute.
		stageCtx := e.getShutdownCtx()
		if stageCtx == nil {
			stageCtx = context.Background()
		}
		upstream := initialCtx
		for i := startIdx + 1; i < len(daemonSpec.Trigger.Before); i++ {
			if e.isShuttingDown() {
				e.log.Debug("propagation aborted: engine shutting down",
					zap.String("daemon", daemonSpec.ID),
					zap.Int("stage", i))
				return
			}
			if !e.daemonRegistered(daemonSpec.ID) {
				e.log.Debug("propagation aborted: daemon unregistered",
					zap.String("daemon", daemonSpec.ID),
					zap.Int("stage", i))
				return
			}
			entry := daemonSpec.Trigger.Before[i]
			// parent_run_id is "" here: the daemon's body run is created
			// AFTER preflight finishes via startDaemonInternal's fireAsync,
			// so we have no parent run ID to stamp on stage rows at this
			// layer. See dispatchPipelineStage's parentRunID parameter
			// doc.
			out, err := e.dispatchPipelineStage(stageCtx, entry, i, upstream, "")
			if err != nil {
				e.log.Warn("daemon mid-pipeline re-fire: descendant stage failed; daemon left at current state",
					zap.String("task", daemonSpec.ID),
					zap.Int("stage", i),
					zap.Error(err),
				)
				return
			}
			upstream = out
		}

		// Last guard before tearing down + restarting: a stage could
		// have completed just as the operator unregistered the daemon
		// or the engine started shutting down. Without this, we'd call
		// startDaemonInternal on a daemon Unregister has already
		// purged from daemonSpecs — leaving a stray daemonRuns entry
		// that races the shutdown path.
		if e.isShuttingDown() {
			e.log.Debug("propagation restart skipped: engine shutting down",
				zap.String("daemon", daemonSpec.ID))
			return
		}
		if !e.daemonRegistered(daemonSpec.ID) {
			e.log.Debug("propagation restart skipped: daemon unregistered",
				zap.String("daemon", daemonSpec.ID))
			return
		}

		// All descendants succeeded — stop the current daemon run and
		// fire a fresh one. Skip the preflight re-run on startup
		// because descendants are already fresh from the propagation
		// above; re-running runPrereqs here would double-fire the
		// re-rendered stages and re-execute the (unchanged) earlier
		// stages.
		e.setDaemonState(daemonSpec.ID, DaemonStopping)
		e.daemonMu.Lock()
		runID := e.daemonRuns[daemonSpec.ID]
		e.daemonMu.Unlock()
		if runID != "" {
			e.KillRun(runID)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = e.WaitRun(ctx, runID)
			cancel()
		}
		e.startDaemonInternal(daemonSpec, true)
	}()
}

// restartGate is a per-task at-most-one-in-flight lock. Implemented as a
// sync.Map keyed on task ID; values are unused (the presence/absence of
// the key is the lock state).
type restartGate struct {
	gates sync.Map // map[string]struct{} — presence == locked
}

func newRestartGate() *restartGate { return &restartGate{} }

// tryAcquire returns true if the caller now holds the lock for taskID,
// false if another goroutine already does.
func (g *restartGate) tryAcquire(taskID string) bool {
	_, loaded := g.gates.LoadOrStore(taskID, struct{}{})
	return !loaded
}

// release frees the lock for taskID. Must be called exactly once per
// successful tryAcquire.
func (g *restartGate) release(taskID string) {
	g.gates.Delete(taskID)
}
