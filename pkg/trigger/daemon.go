// This file contains the standalone daemon (always-on task) lifecycle:
// registration, start, exit handling, and the restart policy with
// exponential backoff. Crash-loop detection lives in crashloop.go and
// WebUI state tracking in daemon_state.go.

package trigger

import (
	"context"
	"fmt"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Daemon crash-loop exponential backoff constants.
// A daemon that exits within daemonStableThreshold is considered crashed;
// one that outlasts it is considered stable and resets the backoff.
const (
	daemonBackoffInit     = 1 * time.Second
	daemonBackoffMax      = 30 * time.Second
	daemonStableThreshold = 10 * time.Second
)

func (e *Engine) registerDaemon(spec *task.Spec) {
	e.daemonMu.Lock()
	e.daemonSpecs[spec.ID] = spec
	e.daemonMu.Unlock()

	e.startDaemon(spec)
}

// startDaemon claims the daemon's slot and launches its body; it coalesces
// when a body is already in flight or being started.
func (e *Engine) startDaemon(spec *task.Spec) {
	slot, ok := e.slots.claim(spec.ID)
	if !ok {
		e.log.Debug("daemon start coalesced — a body is already in flight",
			zap.String("task", spec.ID))
		return
	}
	e.launchDaemon(spec, slot)
}

// launchDaemon fires the daemon body on a claimed slot and records the run +
// DaemonRunning state. Daemons that need config rendered before they start are
// expressed as a kind: PipelineTask whose terminal stage is this daemon Task —
// the render stages run in the pipeline runner, not here.
func (e *Engine) launchDaemon(spec *task.Spec, slot daemonSlot) {
	// Record the spawn time before the body can exit: a fast crash would
	// otherwise run noteExit first and a late noteSpawn would stamp a spawn
	// time onto a dead run, which crash-loop recovery misreads as sustained.
	e.crashloops.noteSpawn(spec.ID)

	// Attach the run and publish DaemonRunning before firing the body: an
	// instant crash can drive onDaemonRunFinished before fireAsync returns, and
	// a late write would leave a stale slot or overwrite its terminal state.
	// fireAsync honors a non-empty opts.RunID.
	runID := uuid.New().String()
	if !slot.attach(runID) {
		// Unregistered while the start was in flight.
		e.crashloops.clearSpawn(spec.ID)
		return
	}
	e.setDaemonState(spec.ID, DaemonRunning)

	if _, err := e.fireAsync(context.Background(), spec, pkgruntime.RunOptions{RunID: runID}, registry.TriggerDaemon); err != nil {
		// A launch failure (binary missing, port bound, resource exhaustion) is
		// distinct from DaemonStopped so the WebUI can tell it from a deliberate
		// stop. No run is live, so give the slot back.
		slot.release(runID)
		e.crashloops.clearSpawn(spec.ID)
		e.setDaemonState(spec.ID, DaemonFailedAfterPreflight)
		e.log.Error("daemon start failed",
			zap.String("task", spec.ID),
			zap.Error(err),
		)
		return
	}

	// An Unregister between attach and fireAsync evicted the slot before the
	// run was killable, so its KillRun missed; kill the body here.
	if !slot.bound(runID) {
		e.KillRun(runID)
	}
}

// resumeDaemonBody spawns the continuation of a suspended daemon body. The slot
// stays parked on the suspended run across the gap; it is adopted for the
// continuation before the body fires so a reconciler reload sees a body in
// flight. Adoption fails, and the resume coalesces, if the slot is no longer
// bound to the suspended run (unregistered, or another start took over).
func (e *Engine) resumeDaemonBody(spec *task.Spec, suspendedRunID string, opts pkgruntime.RunOptions) (string, error) {
	runID := uuid.New().String()
	opts.RunID = runID

	slot, ok := e.slots.holding(spec.ID, suspendedRunID)
	if !ok {
		return "", fmt.Errorf("resume: daemon %q slot no longer parked on suspended run %q", spec.ID, suspendedRunID)
	}
	if err := slot.adopt(runID); err != nil {
		return "", fmt.Errorf("resume: daemon %q: %w (suspended run %q)", spec.ID, err, suspendedRunID)
	}

	e.crashloops.noteSpawn(spec.ID)
	e.setDaemonState(spec.ID, DaemonRunning)

	newRunID, err := e.fireAsync(context.Background(), spec, opts, registry.TriggerResume)
	if err != nil {
		slot.release(runID)
		e.crashloops.clearSpawn(spec.ID)
		e.setDaemonState(spec.ID, DaemonFailedAfterPreflight)
		return "", err
	}
	if !slot.bound(runID) {
		e.KillRun(runID)
	}
	return newRunID, nil
}

func (e *Engine) onDaemonRunFinished(spec *task.Spec, runID string) {
	// Resolve the run status BEFORE releasing the slot: a suspended body must
	// keep its slot (see below), so the slot decision depends on
	// the outcome.
	run, err := e.registry.GetRun(context.Background(), runID)
	if err != nil {
		// Free the slot even on a lookup failure so a dead body can't pin the
		// "body in flight" reservation forever.
		e.slots.releaseRun(spec.ID, runID)
		e.log.Error("daemon: failed to get run status", zap.String("run", runID), zap.Error(err))
		return
	}

	// A suspended run is non-terminal: the subprocess exited to await user
	// input, not because the daemon crashed or stopped. Keep the slot claimed so
	// the "one body in flight" invariant holds across the suspended gap — a reconciler reload must not start a second body while
	// this one is parked — and publish DaemonSuspended instead of leaving a
	// stale "running". The resume continuation (resumeDaemonBody) adopts the
	// slot and drives the next transition. Crash-loop tracking and the restart
	// policy are deliberately untouched. The slot-ownership check guards the
	// unregister race: if Unregister already freed the slot (and killed the
	// body), there is nothing to park and no state to publish for a task the
	// engine no longer tracks.
	if run.Status == registry.StatusSuspended {
		if slot, ok := e.slots.holding(spec.ID, runID); ok && slot.park() {
			e.setDaemonState(spec.ID, DaemonSuspended)
		}
		return
	}

	e.slots.releaseRun(spec.ID, runID)
	e.daemonMu.Lock()
	_, stillRegistered := e.daemonSpecs[spec.ID]
	e.daemonMu.Unlock()

	if !stillRegistered || e.isShuttingDown() {
		return
	}

	// Elapsed run time, shared by the crash-loop tracker and the restart
	// backoff below. run.StartedAt is always set when the run opens; run.FinishedAt
	// may be nil on abnormal exit — treat that as an instant crash so the
	// pessimistic branch applies in both consumers.
	var elapsed time.Duration
	if run.FinishedAt != nil {
		elapsed = run.FinishedAt.Sub(run.StartedAt)
	}

	if run.Status == registry.StatusCancelled {
		// Operator-initiated cancellation (e.g. per-run kill button).
		// The daemon was running; treat the deliberate stop as a clean
		// transition to DaemonStopped so the WebUI doesn't pin a stale
		// "Running" pill on a daemon whose body has been killed. This sits ahead of the restart-policy decision because
		// cancellation short-circuits it entirely — a killed daemon
		// stays stopped regardless of restart=always/on-failure/never.
		//
		// Cancellation also clears crash-loop tracking: a kill is
		// deliberate operator intent, not another crashed start, and the
		// stopped daemon must not keep reporting "crashlooping".
		e.crashloops.reset(spec.ID)
		e.setDaemonState(spec.ID, DaemonStopped)
		return
	}

	e.restartDaemonAfterExit(spec, run.Status, elapsed)
}

// restartDaemonAfterExit applies crash-loop accounting and the restart policy
// for a daemon body that has left the running state with a non-cancelled exit,
// re-launching it (after backoff, through the daemon slot) when the
// policy calls for it. The caller must have already released the run slot and
// confirmed the task is still registered and the engine is not shutting down.
//
// Reused by two entry points: the run goroutine's onDaemonRunFinished (in-line,
// where blocking on the backoff is fine) and the timeout sweep of a suspended
// daemon (out-of-band, where the run goroutine is already gone) — so both take
// the identical restart decision.
func (e *Engine) restartDaemonAfterExit(spec *task.Spec, status string, elapsed time.Duration) {
	// Crash-loop accounting: a quick non-success exit bumps the
	// consecutive-failure counter; a clean or sustained exit resets it.
	// Tracked for every non-cancelled exit regardless of restart policy so
	// the rule stays a single invariant; in practice only auto-restarting
	// daemons accumulate enough consecutive starts to trip the threshold.
	if fails := e.crashloops.noteExit(spec.ID, elapsed, status == registry.StatusSuccess); fails == crashloopThreshold {
		e.log.Warn("daemon is crash-looping — status reports 'crashlooping' until a run sustains",
			zap.String("task", spec.ID),
			zap.Int("consecutive_quick_failures", fails),
			zap.Duration("sustain_window", crashloopSustainWindow),
		)
	}

	restart := spec.Trigger.Restart
	if restart == "" {
		restart = "always"
	}
	// noRestartTransition records the terminal state when the engine
	// will NOT spawn an automatic restart for this exit. Status-aware
	// split:
	//   - success exit, no restart  → DaemonStopped (clean shutdown)
	//   - non-success exit, no restart → DaemonCrashed (operator
	//     attention required)
	// Without this, the no-restart branches return without touching
	// daemonStates, leaving the WebUI showing a stale "Running" pill
	// for a daemon that's no longer running.
	noRestartTransition := func() {
		if status == registry.StatusSuccess {
			e.setDaemonState(spec.ID, DaemonStopped)
		} else {
			e.setDaemonState(spec.ID, DaemonCrashed)
		}
	}
	switch restart {
	case "never":
		e.log.Info("daemon exited — restart=never, not restarting",
			zap.String("task", spec.ID), zap.String("status", status))
		noRestartTransition()
		return
	case "on-failure":
		if status != registry.StatusFailure {
			e.log.Info("daemon exited — restart=on-failure, not restarting (no failure)",
				zap.String("task", spec.ID), zap.String("status", status))
			noRestartTransition()
			return
		}
	}

	e.log.Info("daemon exited, scheduling restart",
		zap.String("task", spec.ID),
		zap.String("status", status),
		zap.String("restart", restart),
	)

	// Exponential backoff for crash-looping daemons. Constants are package-level
	// (see daemonBackoffInit etc.) so tests can inspect them without repetition.
	// elapsed was computed above (shared with the crash-loop tracker).
	//
	// Load the current backoff for this daemon (stored across restarts in a
	// sync.Map keyed by task ID). Reset to init after a stable run.
	backoffKey := "daemon-backoff:" + spec.ID
	var backoff time.Duration
	if elapsed >= daemonStableThreshold {
		backoff = daemonBackoffInit
	} else {
		if v, ok := e.daemonBackoffs.Load(backoffKey); ok {
			backoff = v.(time.Duration)
		} else {
			backoff = daemonBackoffInit
		}
	}
	e.daemonBackoffs.Store(backoffKey, backoff)

	e.log.Info("daemon restart backoff",
		zap.String("task", spec.ID),
		zap.Duration("backoff", backoff),
		zap.Duration("elapsed", elapsed),
	)

	shutCtx := e.getShutdownCtx()
	if shutCtx == nil {
		shutCtx = context.Background()
	}
	select {
	case <-shutCtx.Done():
		return
	case <-time.After(backoff):
	}

	// Advance the backoff for next crash (cap at max). Done after the sleep so
	// a shutdown during the sleep doesn't persist a doubled value.
	nextBackoff := backoff * 2
	if nextBackoff > daemonBackoffMax {
		nextBackoff = daemonBackoffMax
	}
	e.daemonBackoffs.Store(backoffKey, nextBackoff)

	if e.isShuttingDown() {
		return
	}

	// Claim the slot: a concurrent starter or a re-register that already
	// completed owns the daemon now, so coalesce without touching any state.
	slot, ok := e.slots.claim(spec.ID)
	if !ok {
		e.log.Debug("daemon restart skipped — a body is already in flight",
			zap.String("task", spec.ID))
		return
	}

	// The backoff sleep is long enough for an Unregister to have landed; it
	// must not resurrect the daemon.
	e.daemonMu.Lock()
	_, stillRegistered := e.daemonSpecs[spec.ID]
	e.daemonMu.Unlock()
	if !stillRegistered {
		slot.release("")
		e.log.Debug("daemon restart skipped — task unregistered during backoff",
			zap.String("task", spec.ID))
		return
	}

	e.log.Info("restarting daemon task", zap.String("task", spec.ID))
	e.launchDaemon(spec, slot)
}

// onDaemonSuspensionSwept routes a standalone daemon body whose suspension
// timed out — swept suspended→cancelled by SweepExpiredSuspensions — back
// through the daemon lifecycle. onDaemonRunFinished parked the slot on
// the suspended run and published DaemonSuspended, expecting a resume to adopt
// the slot; but the body goroutine exited cleanly on suspend, so
// onDaemonRunFinished will never run again for this run. Without this the slot
// stays pinned to a now-cancelled run — registerDaemon reads alreadyRunning and
// a reconciler reload refuses to restart, wedging the daemon and bypassing its
// restart policy until a full process restart.
//
// This does the slot release + restart decision onDaemonRunFinished would have
// done, but out-of-band from the (already gone) run goroutine. It must run in
// its own goroutine: restartDaemonAfterExit blocks on the restart backoff.
func (e *Engine) onDaemonSuspensionSwept(spec *task.Spec, run *registry.Run) {
	// Slot-match guard, identical to onDaemonRunFinished/resumeDaemonBody: only
	// release the slot if it still parks the swept run. The status-guarded sweep
	// UPDATE already ensures the sweep and a resume can't both transition the
	// same run (a resumed run is excluded from the swept set), so a resume that
	// adopted the slot is not in play here — but the guard keeps slot ownership
	// consistent with that outcome regardless.
	if !e.slots.releaseRun(spec.ID, run.ID) {
		return
	}
	e.daemonMu.Lock()
	_, stillRegistered := e.daemonSpecs[spec.ID]
	e.daemonMu.Unlock()

	if !stillRegistered || e.isShuttingDown() {
		// Nothing will restart; clear the parked DaemonSuspended so the WebUI
		// doesn't pin a stale "suspended" pill on a daemon that is gone.
		e.setDaemonState(spec.ID, DaemonStopped)
		return
	}

	// elapsed spans the whole suspended gap (StartedAt → the sweep's
	// finished_at). A real suspension outlives daemonStableThreshold, so the
	// restart backoff resets to init — a timeout is not a crash loop.
	var elapsed time.Duration
	if run.FinishedAt != nil {
		elapsed = run.FinishedAt.Sub(run.StartedAt)
	}
	e.restartDaemonAfterExit(spec, run.Status, elapsed)
}
