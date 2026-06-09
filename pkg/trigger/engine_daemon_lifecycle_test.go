package trigger

// Daemon lifecycle tests + shared controllable-executor helpers.
//
// This file holds the surviving daemon-machinery coverage migrated out of the
// (removed) engine_preflight_test.go when trigger.before was deleted in PR6:
//
//   - DaemonState map round-trip;
//   - a daemon with no orchestration starts immediately on registration;
//   - daemon-body launch failure surfaces as DaemonFailedAfterPreflight;
//   - onDaemonRunFinished state transitions (issues #325/#329/#332).
//
// It also defines the controllable preflightExec + newPreflightEnv + waitUntil
// helpers still used by the chain per-edge override test and the pipeline
// runner tests. (The name "preflight" is historical — these helpers drive any
// task through a synchronous, scriptable executor; they no longer have
// anything to do with the removed trigger.before preflight pipeline.)

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// preflightExec is a controllable Executor for the daemon-lifecycle tests.
// One-shot tasks finish StatusSuccess synchronously; tasks marked via
// markDaemon block until their run context is cancelled (KillRun path).
type preflightExec struct {
	mu     sync.Mutex
	daemon map[string]chan struct{} // daemon taskID → block channel
	runs   atomic.Int32             // count of executed runs (any task)
}

func newPreflightExec(_ *registry.Registry) *preflightExec {
	return &preflightExec{
		daemon: make(map[string]chan struct{}),
	}
}

// markDaemon flags a task ID as a long-running daemon. Subsequent Execute
// calls for that task block until the run's context is cancelled (i.e. until
// KillRun is invoked or the engine shuts down).
func (p *preflightExec) markDaemon(taskID string) {
	p.mu.Lock()
	if p.daemon[taskID] == nil {
		p.daemon[taskID] = make(chan struct{})
	}
	p.mu.Unlock()
}

func (p *preflightExec) Execute(ctx context.Context, spec *task.Spec, _ pkgruntime.RunOptions) (*pkgruntime.RunResult, error) {
	p.runs.Add(1)
	p.mu.Lock()
	daemonCh := p.daemon[spec.ID]
	p.mu.Unlock()

	if daemonCh != nil {
		// Long-running daemon: block until the run's context is cancelled
		// (KillRun path). Mirrors how a real daemon exits gracefully on shutdown.
		<-ctx.Done()
		return &pkgruntime.RunResult{}, nil
	}
	return &pkgruntime.RunResult{}, nil
}

// newPreflightEnv returns a test engine + reg + controllable executor for the
// daemon-lifecycle tests. Tasks complete synchronously (or block, for marked
// daemons) without spawning Deno or Docker — we register the same preflightExec
// for both the deno and docker runtimes so daemon specs (typically runtime:
// docker for container daemons) dispatch without needing the real docker
// manager.
func newPreflightEnv(t *testing.T) (*Engine, *registry.Registry, *preflightExec) {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)
	exec := newPreflightExec(reg)
	eng := New(reg, exec, zap.NewNop())
	eng.RegisterExecutor(task.RuntimeDocker, exec)
	return eng, reg, exec
}

func waitUntil(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting: %s", msg)
}

func TestDaemonState_Default(t *testing.T) {
	e := newTestEnv(t)
	if got := e.engine.DaemonState("unknown"); got != DaemonStopped {
		t.Errorf("unknown daemon state = %q, want stopped", got)
	}
}

// TestDaemonState_SetGet verifies that setDaemonState/DaemonState round-trip
// the enum values without contention.
func TestDaemonState_SetGet(t *testing.T) {
	e := newTestEnv(t)
	states := []DaemonState{
		DaemonRunning,
		DaemonStopping,
		DaemonFailedAfterPreflight,
		DaemonCrashed,
		DaemonStopped,
	}
	for _, want := range states {
		e.engine.setDaemonState("d", want)
		if got := e.engine.DaemonState("d"); got != want {
			t.Errorf("setDaemonState(%q) → DaemonState = %q, want %q", want, got, want)
		}
	}
}

// TestDaemon_StartsImmediately verifies a daemon (which no longer has any
// trigger.before orchestration) reaches Running directly on registration.
func TestDaemon_StartsImmediately(t *testing.T) {
	eng, reg, exec := newPreflightEnv(t)
	daemon := &task.Spec{
		ID:      "d-noprereq",
		Name:    "d-noprereq",
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{Daemon: true, Restart: "never"},
		Enabled: true,
	}
	if err := reg.Register(daemon); err != nil {
		t.Fatalf("reg.Register: %v", err)
	}
	exec.markDaemon("d-noprereq")

	if err := eng.Register(daemon); err != nil {
		t.Fatalf("eng.Register: %v", err)
	}
	waitUntil(t, 5*time.Second, func() bool {
		return eng.DaemonState("d-noprereq") == DaemonRunning
	}, "daemon never reached Running")
}

// TestStartDaemon_FireAsyncFailure pins issue #318: when the daemon body's
// launch fails (here forced by closing the DB so startRun errors), the engine
// records DaemonFailedAfterPreflight so the WebUI can distinguish "daemon body
// broke" from "deliberately stopped / never started".
func TestStartDaemon_FireAsyncFailure(t *testing.T) {
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	reg := registry.New(d)
	exec := newPreflightExec(reg)
	eng := New(reg, exec, zap.NewNop())
	eng.RegisterExecutor(task.RuntimeDocker, exec)

	daemon := &task.Spec{
		ID:      "d",
		Name:    "d",
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{Daemon: true, Restart: "never"},
		Enabled: true,
	}
	if err := reg.Register(daemon); err != nil {
		t.Fatalf("reg.Register: %v", err)
	}

	// Close the DB so the next registry write (startRun inside fireAsync)
	// fails. Mirrors disk-full / severed-connection mid-operation.
	if err := d.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}

	// startDaemon is synchronous: fireAsync returns the error inline, so we
	// can assert the state immediately without polling.
	eng.startDaemon(daemon)

	if got := eng.DaemonState("d"); got != DaemonFailedAfterPreflight {
		t.Fatalf("DaemonState = %q, want %q (daemon-body launch failure must distinguish from deliberately-stopped)",
			got, DaemonFailedAfterPreflight)
	}
	// A daemon that was never started must NOT appear as failed_after_preflight.
	if got := eng.DaemonState("never-touched"); got != DaemonStopped {
		t.Errorf("DaemonState(unknown) = %q, want %q", got, DaemonStopped)
	}
}

// finishedRunEnv stages a daemon spec + a finished registry run so the
// onDaemonRunFinished tests can call the hook directly without going through
// the full fireAsync → daemon-execute path.
func finishedRunEnv(t *testing.T, restartPolicy string, status string) (*Engine, *task.Spec, string) {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)
	exec := newPreflightExec(reg)
	eng := New(reg, exec, zap.NewNop())
	eng.RegisterExecutor(task.RuntimeDocker, exec)

	spec := &task.Spec{
		ID:      "d",
		Name:    "d",
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{Daemon: true, Restart: restartPolicy},
		Enabled: true,
	}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("reg.Register: %v", err)
	}
	// Mark the daemon as still-registered with the engine so the
	// onDaemonRunFinished early-return guard doesn't bail out.
	eng.daemonMu.Lock()
	eng.daemonSpecs[spec.ID] = spec
	eng.daemonMu.Unlock()

	// Seed a run row and finalize it with the requested terminal status —
	// onDaemonRunFinished looks the run up via GetRun to decide whether to
	// restart.
	ctx := context.Background()
	runID, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := reg.FinishRun(ctx, runID, status); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	// Pre-seed daemonStates with DaemonRunning so we can assert the
	// transition actually fires (rather than the default "stopped"
	// fall-through). Mirrors reality: the stale-Running pill in issue #325
	// only appears because the engine set DaemonRunning before the body
	// crashed.
	eng.setDaemonState(spec.ID, DaemonRunning)
	return eng, spec, runID
}

// TestOnDaemonRunFinished_NoRestartFailure_TransitionsToCrashed — issue #325:
// a daemon whose body exits with status=failure under restart:never must
// transition to DaemonCrashed (operator attention) not a stale Running pill.
func TestOnDaemonRunFinished_NoRestartFailure_TransitionsToCrashed(t *testing.T) {
	eng, spec, runID := finishedRunEnv(t, "never", registry.StatusFailure)
	eng.onDaemonRunFinished(spec, runID)
	if got := eng.DaemonState(spec.ID); got != DaemonCrashed {
		t.Fatalf("DaemonState = %q, want %q (non-success exit + no restart must surface as Crashed)",
			got, DaemonCrashed)
	}
}

// TestOnDaemonRunFinished_NoRestartSuccess_TransitionsToStopped — clean-exit
// half of #325: a successful exit under restart:never is a clean shutdown.
func TestOnDaemonRunFinished_NoRestartSuccess_TransitionsToStopped(t *testing.T) {
	eng, spec, runID := finishedRunEnv(t, "never", registry.StatusSuccess)
	eng.onDaemonRunFinished(spec, runID)
	if got := eng.DaemonState(spec.ID); got != DaemonStopped {
		t.Fatalf("DaemonState = %q, want %q (clean exit + no restart must surface as Stopped)",
			got, DaemonStopped)
	}
}

// TestOnDaemonRunFinished_OnFailurePolicy_SuccessExit_TransitionsToStopped —
// restart=on-failure with a success exit: nothing to restart, lands Stopped.
func TestOnDaemonRunFinished_OnFailurePolicy_SuccessExit_TransitionsToStopped(t *testing.T) {
	eng, spec, runID := finishedRunEnv(t, "on-failure", registry.StatusSuccess)
	eng.onDaemonRunFinished(spec, runID)
	if got := eng.DaemonState(spec.ID); got != DaemonStopped {
		t.Fatalf("DaemonState = %q, want %q (on-failure + clean exit must surface as Stopped)",
			got, DaemonStopped)
	}
}

// TestOnDaemonRunFinished_Cancelled_RestartNever_TransitionsToStopped — #332:
// operator-initiated cancellation under restart:never is a clean stop.
func TestOnDaemonRunFinished_Cancelled_RestartNever_TransitionsToStopped(t *testing.T) {
	eng, spec, runID := finishedRunEnv(t, "never", registry.StatusCancelled)
	eng.onDaemonRunFinished(spec, runID)
	if got := eng.DaemonState(spec.ID); got != DaemonStopped {
		t.Fatalf("DaemonState = %q, want %q (operator cancellation must surface as Stopped)",
			got, DaemonStopped)
	}
}

// TestOnDaemonRunFinished_Cancelled_RestartAlways_TransitionsToStopped — #332:
// cancellation is operator intent, so even restart:always must NOT respawn.
func TestOnDaemonRunFinished_Cancelled_RestartAlways_TransitionsToStopped(t *testing.T) {
	eng, spec, runID := finishedRunEnv(t, "always", registry.StatusCancelled)
	eng.onDaemonRunFinished(spec, runID)
	if got := eng.DaemonState(spec.ID); got != DaemonStopped {
		t.Fatalf("DaemonState = %q, want %q (operator cancellation overrides restart=always)",
			got, DaemonStopped)
	}
}

// TestOnDaemonRunFinished_Cancelled_RestartOnFailure_TransitionsToStopped —
// cancellation under restart=on-failure must not leak into the
// failure-classification path.
func TestOnDaemonRunFinished_Cancelled_RestartOnFailure_TransitionsToStopped(t *testing.T) {
	eng, spec, runID := finishedRunEnv(t, "on-failure", registry.StatusCancelled)
	eng.onDaemonRunFinished(spec, runID)
	if got := eng.DaemonState(spec.ID); got != DaemonStopped {
		t.Fatalf("DaemonState = %q, want %q (cancellation under restart=on-failure must surface as Stopped)",
			got, DaemonStopped)
	}
}
