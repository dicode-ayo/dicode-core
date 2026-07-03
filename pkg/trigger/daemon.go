// This file contains the standalone daemon (always-on task) lifecycle:
// registration, start, exit handling, and the restart policy with
// exponential backoff. Crash-loop detection lives in crashloop.go and
// WebUI state tracking in daemon_state.go.

package trigger

import (
	"context"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
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
	// Release happens inside startDaemon after the daemon's run slot is
	// recorded (or after a dispatch failure has been logged).
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
	runID, err := e.fireAsync(context.Background(), spec, pkgruntime.RunOptions{}, registry.TriggerDaemon)
	if err != nil {
		// fireAsync error here is a daemon-body launch failure — binary
		// missing, port already bound, runtime resource exhaustion. Distinct
		// from DaemonStopped so operators can tell "deliberately stopped /
		// never started" apart from "daemon body broke" in the WebUI. See
		// issue #318.
		//
		// No run is live, so drop the spawn timestamp recorded above — it
		// must not age into a fake "sustained run".
		e.crashloops.clearSpawn(spec.ID)
		e.setDaemonState(spec.ID, DaemonFailedAfterPreflight)
		e.log.Error("daemon start failed",
			zap.String("task", spec.ID),
			zap.Error(err),
		)
		return
	}
	e.daemonMu.Lock()
	e.daemonRuns[spec.ID] = runID
	e.daemonMu.Unlock()
	e.setDaemonState(spec.ID, DaemonRunning)
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

	if !e.isShuttingDown() {
		e.log.Info("restarting daemon task", zap.String("task", spec.ID))
		e.startDaemon(spec)
	}
}
