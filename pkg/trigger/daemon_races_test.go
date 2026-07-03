package trigger

// Daemon-lifecycle race regression tests (issue #470).
//
// Two pre-existing races surfaced by the #469 decomposition review:
//
//   Race 1 — startDaemon used to write daemonRuns[spec.ID] and set
//   DaemonRunning only AFTER fireAsync had already launched the run
//   goroutine. An instant crash could drive onDaemonRunFinished before the
//   slot write: the finish path's `daemonRuns[spec.ID] == runID` cleanup
//   missed (stale slot left behind) and the late setDaemonState(Running)
//   overwrote the terminal DaemonStopped/DaemonCrashed. Fixed by
//   pre-generating the run ID (fireAsync honors a caller-provided
//   opts.RunID) and reserving slot + Running state before the body fires.
//
//   Race 2 — the backoff-restart tail of onDaemonRunFinished called
//   startDaemon without acquiring restartGates, so a re-register during a
//   pending backoff could double-start the same task body. Fixed by routing
//   the restart through the same gate as registerDaemon, plus re-checking
//   registration and the run slot after the backoff sleep.
//
// Determinism notes: the interleavings are produced structurally, not by
// timing. For race 1, the crashDaemonExec inspects the daemonRuns slot at
// body START — the reservation is now made before fireAsync is even called,
// so the body must always observe it. For race 2, the test acquires the
// restart gate itself (playing the concurrent re-register) and holds it
// across the entire backoff window, so the restart path deterministically
// hits the held gate.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// crashDaemonExec is an Executor whose runs complete IMMEDIATELY with a
// failure — the instant-crash daemon body from race 1. At body start it
// checks whether the engine's daemonRuns slot already carries this run's ID,
// recording how many runs observed their reservation.
type crashDaemonExec struct {
	eng            *Engine
	runs           atomic.Int32 // total bodies executed
	reservedAtBody atomic.Int32 // bodies that saw daemonRuns[spec.ID] == opts.RunID at start
}

func (c *crashDaemonExec) Execute(_ context.Context, spec *task.Spec, opts pkgruntime.RunOptions) (*pkgruntime.RunResult, error) {
	c.runs.Add(1)
	c.eng.daemonMu.Lock()
	reserved := c.eng.daemonRuns[spec.ID] == opts.RunID
	c.eng.daemonMu.Unlock()
	if reserved {
		c.reservedAtBody.Add(1)
	}
	return &pkgruntime.RunResult{RunID: opts.RunID, Error: errors.New("instant crash")}, nil
}

// daemonRaceEnv returns a test engine wired to a crashDaemonExec, with the
// given daemon spec registered in the registry AND seeded into daemonSpecs
// (so onDaemonRunFinished's stillRegistered guard doesn't bail out). Mirrors
// newPreflightEnv / crashloopEnv.
func daemonRaceEnv(t *testing.T, restartPolicy string) (*Engine, *task.Spec, *crashDaemonExec) {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)
	exec := &crashDaemonExec{}
	eng := New(reg, exec, zap.NewNop())
	eng.RegisterExecutor(task.RuntimeDocker, exec)
	exec.eng = eng

	spec := &task.Spec{
		ID:      "d-race",
		Name:    "d-race",
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{Daemon: true, Restart: restartPolicy},
		Enabled: true,
	}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("reg.Register: %v", err)
	}
	eng.daemonMu.Lock()
	eng.daemonSpecs[spec.ID] = spec
	eng.daemonMu.Unlock()
	return eng, spec, exec
}

// TestFireAsync_HonorsCallerRunID pins the mechanism race 1 relies on: a
// non-empty opts.RunID must be used verbatim (generated only when empty), so
// startDaemon can reserve the daemonRuns slot before firing the body.
func TestFireAsync_HonorsCallerRunID(t *testing.T) {
	eng, reg, _ := newPreflightEnv(t)
	spec := &task.Spec{
		ID:      "oneshot",
		Name:    "oneshot",
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Enabled: true,
	}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("reg.Register: %v", err)
	}

	want := uuid.New().String()
	got, err := eng.fireAsync(context.Background(), spec, pkgruntime.RunOptions{RunID: want}, "test")
	if err != nil {
		t.Fatalf("fireAsync: %v", err)
	}
	if got != want {
		t.Fatalf("fireAsync returned run ID %q, want caller-provided %q", got, want)
	}
	// The run record must exist under the caller-provided ID.
	if _, err := reg.GetRun(context.Background(), want); err != nil {
		t.Fatalf("GetRun(%q): %v", want, err)
	}

	// And the empty-RunID path still generates one.
	gen, err := eng.fireAsync(context.Background(), spec, pkgruntime.RunOptions{}, "test")
	if err != nil {
		t.Fatalf("fireAsync (generated): %v", err)
	}
	if gen == "" || gen == want {
		t.Fatalf("fireAsync with empty RunID returned %q, want a fresh generated ID", gen)
	}
}

// TestStartDaemon_InstantCrash_NoStaleSlot_TerminalStateSticks — race 1: a
// daemon body that exits before startDaemon regains control must still find
// its reservation. Assert: the body observed daemonRuns[spec.ID] == its run
// ID at start; after the terminal transition no stale daemonRuns entry
// remains; and with restart:never the terminal DaemonCrashed sticks —
// DaemonState never reports Running again.
func TestStartDaemon_InstantCrash_NoStaleSlot_TerminalStateSticks(t *testing.T) {
	eng, spec, exec := daemonRaceEnv(t, "never")

	eng.startDaemon(spec)

	waitUntil(t, 5*time.Second, func() bool {
		return eng.DaemonState(spec.ID) == DaemonCrashed
	}, "daemon never reached the terminal DaemonCrashed state")

	if runs, reserved := exec.runs.Load(), exec.reservedAtBody.Load(); runs != 1 || reserved != runs {
		t.Fatalf("runs=%d, bodies that observed their reservation=%d — the run slot must be written before the body fires",
			runs, reserved)
	}

	eng.daemonMu.Lock()
	staleRun, stale := eng.daemonRuns[spec.ID]
	eng.daemonMu.Unlock()
	if stale {
		t.Fatalf("stale daemonRuns entry %q left behind after the daemon body finished", staleRun)
	}

	// The terminal state must stick: startDaemon has nothing left to do after
	// fireAsync, so no late DaemonRunning can overwrite DaemonCrashed.
	time.Sleep(100 * time.Millisecond)
	if got := eng.DaemonState(spec.ID); got != DaemonCrashed {
		t.Fatalf("DaemonState = %q after terminal transition, want %q (a late Running overwrite is race 1)",
			got, DaemonCrashed)
	}
}
