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

// Daemon crash-loop exponential backoff constants (Fix 4, #387).
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
	_, alreadyRunning := e.daemonRuns[spec.ID]
	e.daemonMu.Unlock()

	if alreadyRunning {
		return
	}
	// Gate the start path with the same per-daemon lock used for restart
	// coalescing. Two concurrent registerDaemon calls (e.g. the reconciler
	// firing OnRegister twice, or a manual re-register racing the reconciler)
	// would otherwise both observe `alreadyRunning=false` and both spawn a
	// daemon body. The lock ensures at most one in-flight start per task ID.
	// The backoff-restart path in onDaemonRunFinished routes through the same
	// gate (#470, race 2). Released once startDaemon has recorded the daemon's
	// run slot (or logged a launch failure).
	if !e.restartGates.tryAcquire(spec.ID) {
		e.log.Debug("daemon start coalesced — another start is already in flight",
			zap.String("task", spec.ID))
		return
	}
	defer e.restartGates.release(spec.ID)
	e.startDaemon(spec)
}

// startDaemon brings a daemon up: it fires the daemon body and records the
// run + DaemonRunning state. Daemons that need config rendered before they
// start are expressed as a kind: PipelineTask whose terminal stage is this
// daemon Task — the render stages run in the pipeline runner, not here.
func (e *Engine) startDaemon(spec *task.Spec) {
	// Record the spawn time for lazy crash-loop recovery BEFORE the body can
	// possibly exit: fireAsync launches the run goroutine, so a very fast
	// crash could otherwise run noteExit first and a late noteSpawn would
	// stamp a spawn time onto a dead run — which isCrashLooping's lazy
	// recovery would later misread as a sustained run and wrongly clear the
	// crash-loop state (#458).
	e.crashloops.noteSpawn(spec.ID)

	// Reserve the run slot and publish DaemonRunning BEFORE firing the body
	// (#470, race 1). fireAsync launches the run goroutine and returns; an
	// instant crash can drive onDaemonRunFinished before control returns
	// here. If the slot were written after fireAsync (as it used to be), the
	// finish path's `daemonRuns[spec.ID] == runID` cleanup would miss —
	// leaving a stale slot that makes registerDaemon think the daemon is
	// still up — and the late setDaemonState(DaemonRunning) would overwrite
	// the terminal DaemonStopped/DaemonCrashed the finish path just
	// recorded. Pre-generating the run ID (fireAsync honors a non-empty
	// opts.RunID) makes the reservation observable before the body can
	// possibly exit, and leaves nothing to do after fireAsync succeeds.
	runID := uuid.New().String()
	e.daemonMu.Lock()
	e.daemonRuns[spec.ID] = runID
	e.daemonMu.Unlock()
	e.setDaemonState(spec.ID, DaemonRunning)

	if _, err := e.fireAsync(context.Background(), spec, pkgruntime.RunOptions{RunID: runID}, registry.TriggerDaemon); err != nil {
		// fireAsync error here is a daemon-body launch failure — binary
		// missing, port already bound, runtime resource exhaustion. Distinct
		// from DaemonStopped so operators can tell "deliberately stopped /
		// never started" apart from "daemon body broke" in the WebUI. See
		// issue #318.
		//
		// No run is live: roll back the reservation made above. The
		// slot-matches guard mirrors onDaemonRunFinished so a concurrent
		// starter's fresh reservation is never clobbered.
		e.daemonMu.Lock()
		if e.daemonRuns[spec.ID] == runID {
			delete(e.daemonRuns, spec.ID)
		}
		e.daemonMu.Unlock()
		// Drop the spawn timestamp recorded above — it must not age into a
		// fake "sustained run".
		e.crashloops.clearSpawn(spec.ID)
		e.setDaemonState(spec.ID, DaemonFailedAfterPreflight)
		e.log.Error("daemon start failed",
			zap.String("task", spec.ID),
			zap.Error(err),
		)
	}
}

// resumeDaemonBody spawns the continuation of a suspended daemon body while
// keeping the #470 "slot present == one body in flight" invariant honest across
// the suspend→resume hop. onDaemonRunFinished parked the slot on the suspended
// run; this adopts it for the continuation BEFORE the body fires (mirroring
// startDaemon's pre-reservation, which fireAsync honors via opts.RunID) so a
// reconciler reload during the continuation observes alreadyRunning and won't
// start a second body, and the continuation's onDaemonRunFinished slot-match
// frees the slot correctly. The compare-and-swap is the safety interlock: if
// the slot is no longer parked on the suspended run (the task was unregistered,
// or another start took over), coalesce rather than double-start.
func (e *Engine) resumeDaemonBody(spec *task.Spec, suspendedRunID string, opts pkgruntime.RunOptions) (string, error) {
	runID := uuid.New().String()
	opts.RunID = runID

	e.daemonMu.Lock()
	if e.daemonRuns[spec.ID] != suspendedRunID {
		e.daemonMu.Unlock()
		return "", fmt.Errorf("resume: daemon %q slot no longer parked on suspended run %q", spec.ID, suspendedRunID)
	}
	e.daemonRuns[spec.ID] = runID
	e.daemonMu.Unlock()

	// Record the spawn before fireAsync for the same reason startDaemon does:
	// an instant exit could otherwise run noteExit before a late noteSpawn,
	// stamping a spawn time onto a dead run (#458).
	e.crashloops.noteSpawn(spec.ID)
	e.setDaemonState(spec.ID, DaemonRunning)

	newRunID, err := e.fireAsync(context.Background(), spec, opts, registry.TriggerResume)
	if err != nil {
		// No body is live: roll back the adoption. The slot-match guard mirrors
		// onDaemonRunFinished so a concurrent starter's reservation is never
		// clobbered.
		e.daemonMu.Lock()
		if e.daemonRuns[spec.ID] == runID {
			delete(e.daemonRuns, spec.ID)
		}
		e.daemonMu.Unlock()
		e.crashloops.clearSpawn(spec.ID)
		e.setDaemonState(spec.ID, DaemonFailedAfterPreflight)
		return "", err
	}
	return newRunID, nil
}

func (e *Engine) onDaemonRunFinished(spec *task.Spec, runID string) {
	// Resolve the run status BEFORE releasing the slot: a suspended body must
	// keep its #470 reservation (see below), so the slot decision depends on
	// the outcome.
	run, err := e.registry.GetRun(context.Background(), runID)
	if err != nil {
		// Free the slot even on a lookup failure so a dead body can't pin the
		// "body in flight" reservation forever.
		e.daemonMu.Lock()
		if e.daemonRuns[spec.ID] == runID {
			delete(e.daemonRuns, spec.ID)
		}
		e.daemonMu.Unlock()
		e.log.Error("daemon: failed to get run status", zap.String("run", runID), zap.Error(err))
		return
	}

	// A suspended run is non-terminal (#95): the subprocess exited to await
	// user input, not because the daemon crashed or stopped. Keep the run slot
	// reserved so the #470 "one body in flight" invariant holds across the
	// suspended gap — a reconciler reload must not start a second body while
	// this one is parked — and publish DaemonSuspended instead of leaving a
	// stale "running". The resume continuation (resumeDaemonBody) adopts the
	// slot and drives the next transition. Crash-loop tracking and the restart
	// policy are deliberately untouched. The slot-ownership check guards the
	// unregister race: if Unregister already freed the slot (and killed the
	// body), there is nothing to park and no state to publish for a task the
	// engine no longer tracks.
	if run.Status == registry.StatusSuspended {
		e.daemonMu.Lock()
		slotOwned := e.daemonRuns[spec.ID] == runID
		e.daemonMu.Unlock()
		if slotOwned {
			e.setDaemonState(spec.ID, DaemonSuspended)
		}
		return
	}

	e.daemonMu.Lock()
	if e.daemonRuns[spec.ID] == runID {
		delete(e.daemonRuns, spec.ID)
	}
	_, stillRegistered := e.daemonSpecs[spec.ID]
	e.daemonMu.Unlock()

	if !stillRegistered || e.isShuttingDown() {
		return
	}

	// Elapsed run time, shared by the crash-loop tracker and the restart
	// backoff below. run.StartedAt is always set by startRun; run.FinishedAt
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
		// "Running" pill on a daemon whose body has been killed (issue
		// #332). This sits ahead of the noRestartTransition switch
		// below because cancellation short-circuits the restart-policy
		// decision entirely — a killed daemon stays stopped regardless
		// of restart=always/on-failure/never.
		//
		// Cancellation also clears crash-loop tracking (#458): a kill is
		// deliberate operator intent, not another crashed start, and the
		// stopped daemon must not keep reporting "crashlooping".
		e.crashloops.reset(spec.ID)
		e.setDaemonState(spec.ID, DaemonStopped)
		return
	}

	// Crash-loop accounting (#458): a quick non-success exit bumps the
	// consecutive-failure counter; a clean or sustained exit resets it.
	// Tracked for every non-cancelled exit regardless of restart policy so
	// the rule stays a single invariant; in practice only auto-restarting
	// daemons accumulate enough consecutive starts to trip the threshold.
	if fails := e.crashloops.noteExit(spec.ID, elapsed, run.Status == registry.StatusSuccess); fails == crashloopThreshold {
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
	// split (issue #325):
	//   - success exit, no restart  → DaemonStopped (clean shutdown)
	//   - non-success exit, no restart → DaemonCrashed (operator
	//     attention required)
	// Without this, the no-restart branches return without touching
	// daemonStates, leaving the WebUI showing a stale "Running" pill
	// for a daemon that's no longer running.
	noRestartTransition := func() {
		if run.Status == registry.StatusSuccess {
			e.setDaemonState(spec.ID, DaemonStopped)
		} else {
			e.setDaemonState(spec.ID, DaemonCrashed)
		}
	}
	switch restart {
	case "never":
		e.log.Info("daemon exited — restart=never, not restarting",
			zap.String("task", spec.ID), zap.String("status", run.Status))
		noRestartTransition()
		return
	case "on-failure":
		if run.Status != registry.StatusFailure {
			e.log.Info("daemon exited — restart=on-failure, not restarting (no failure)",
				zap.String("task", spec.ID), zap.String("status", run.Status))
			noRestartTransition()
			return
		}
	}

	e.log.Info("daemon exited, scheduling restart",
		zap.String("task", spec.ID),
		zap.String("status", run.Status),
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

	// Route the backoff restart through the same per-daemon gate as
	// registerDaemon (#470, race 2). Without it, a re-register during the
	// backoff sleep (e.g. the reconciler firing OnRegister after a content
	// reload) could start the daemon body while this goroutine also calls
	// startDaemon — a double-start of the same task body. When the gate is
	// held, the concurrent starter owns the daemon now: coalesce and exit
	// without touching any state.
	if !e.restartGates.tryAcquire(spec.ID) {
		e.log.Debug("daemon start coalesced — another start is already in flight",
			zap.String("task", spec.ID))
		return
	}
	defer e.restartGates.release(spec.ID)

	// Re-check under the gate: the backoff sleep is up to daemonBackoffMax,
	// plenty of time for the world to have moved on.
	//   - An Unregister during the sleep must not resurrect the daemon (the
	//     stillRegistered check at the top of this function predates the
	//     sleep, so it cannot cover this).
	//   - A re-register that already completed (gate released) has a live
	//     run slot; starting again would double-start the body. With the
	//     slot now reserved before the body fires (race 1 fix above), slot
	//     presence is a reliable "body in flight" signal — the same check
	//     registerDaemon uses.
	e.daemonMu.Lock()
	_, stillRegistered = e.daemonSpecs[spec.ID]
	_, alreadyRunning := e.daemonRuns[spec.ID]
	e.daemonMu.Unlock()
	if !stillRegistered {
		e.log.Debug("daemon restart skipped — task unregistered during backoff",
			zap.String("task", spec.ID))
		return
	}
	if alreadyRunning {
		e.log.Debug("daemon restart skipped — daemon already restarted concurrently",
			zap.String("task", spec.ID))
		return
	}

	e.log.Info("restarting daemon task", zap.String("task", spec.ID))
	e.startDaemon(spec)
}
