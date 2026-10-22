package trigger

// Daemon-lifecycle race tests.
//
// Start race: startDaemon claims the slot, binds the run ID and publishes
// DaemonRunning before the body fires (fireAsync honors a caller-provided
// opts.RunID), so an instant crash that drives onDaemonRunFinished before
// startDaemon regains control still finds its slot and cannot be overwritten
// by a late DaemonRunning.
//
// Restart race: the backoff restart claims the same slot as registerDaemon and
// re-checks registration after the sleep, so a concurrent start or an
// Unregister during the backoff is never double-started or resurrected.
//
// The interleavings are produced structurally, not by timing: crashDaemonExec
// inspects the slot at body start, and the restart tests claim or seed the
// slot themselves to play the concurrent starter.

import (
	"context"
	"errors"
	"sync"
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
// failure — an instant-crash daemon body. At body start it
// checks whether the engine's daemon slot already carries this run's ID,
// recording how many runs observed their slot binding.
type crashDaemonExec struct {
	eng            *Engine
	runs           atomic.Int32 // total bodies executed
	reservedAtBody atomic.Int32 // bodies that saw slot run == opts.RunID at start
}

func (c *crashDaemonExec) Execute(_ context.Context, spec *task.Spec, opts pkgruntime.RunOptions) (*pkgruntime.RunResult, error) {
	c.runs.Add(1)
	reserved := c.eng.slots.runID(spec.ID) == opts.RunID
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

// TestFireAsync_HonorsCallerRunID pins the mechanism the start race relies on: a
// non-empty opts.RunID must be used verbatim (generated only when empty), so
// startDaemon can bind the daemon slot before firing the body.
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

// TestStartDaemon_InstantCrash_NoStaleSlot_TerminalStateSticks — a daemon body
// that exits before startDaemon regains control must still find its slot.
// Assert: the body saw the slot bound to its run ID at start; after the
// terminal transition no slot remains; and with restart:never the terminal
// DaemonCrashed sticks — DaemonState never reports Running again.
func TestStartDaemon_InstantCrash_NoStaleSlot_TerminalStateSticks(t *testing.T) {
	eng, spec, exec := daemonRaceEnv(t, "never")

	eng.startDaemon(spec)

	waitUntil(t, 5*time.Second, func() bool {
		return eng.DaemonState(spec.ID) == DaemonCrashed
	}, "daemon never reached the terminal DaemonCrashed state")

	if runs, reserved := exec.runs.Load(), exec.reservedAtBody.Load(); runs != 1 || reserved != runs {
		t.Fatalf("runs=%d, bodies that observed their slot binding=%d — the slot must be bound before the body fires",
			runs, reserved)
	}

	if staleRun := eng.slots.runID(spec.ID); staleRun != "" {
		t.Fatalf("stale daemon slot entry %q left behind after the daemon body finished", staleRun)
	}

	// The terminal state must stick: startDaemon has nothing left to do after
	// fireAsync, so no late DaemonRunning can overwrite DaemonCrashed.
	time.Sleep(100 * time.Millisecond)
	if got := eng.DaemonState(spec.ID); got != DaemonCrashed {
		t.Fatalf("DaemonState = %q after terminal transition, want %q (a late Running overwrite)",
			got, DaemonCrashed)
	}
}

// TestDaemon_BackoffRestartCoalescesWithConcurrentStart — end to end: a real
// startDaemon runs an instant-crash body; once its slot is released for the
// backoff, a concurrent starter (a re-register) claims the slot and holds it
// across the backoff. The restart must coalesce on the held slot, so exactly
// one body ever runs.
func TestDaemon_BackoffRestartCoalescesWithConcurrentStart(t *testing.T) {
	eng, spec, exec := daemonRaceEnv(t, "always")

	eng.startDaemon(spec)

	var slot daemonSlot
	waitUntil(t, 5*time.Second, func() bool {
		if exec.runs.Load() < 1 {
			return false
		}
		s, ok := eng.slots.claim(spec.ID)
		slot = s
		return ok
	}, "could not claim the slot while the crashed body was backing off")
	defer slot.release("")

	time.Sleep(daemonBackoffInit + 1500*time.Millisecond)
	if got := exec.runs.Load(); got != 1 {
		t.Fatalf("executed %d daemon bodies, want exactly 1 — the backoff restart must coalesce on the held slot", got)
	}
}

// TestOnDaemonRunFinished_BackoffRestart_SkipsWhenAlreadyRestarted — completed-restart
// flavor: by the time the backoff elapses, a re-register
// has already started a fresh body (live daemon slot). The restart path must observe the slot and skip rather than start a second
// body next to the live one.
func TestOnDaemonRunFinished_BackoffRestart_SkipsWhenAlreadyRestarted(t *testing.T) {
	eng, spec, exec := daemonRaceEnv(t, "always")

	// Seed a finished quick-failure run for the OLD body.
	runID := quickFailureRun(t, eng, spec.ID, registry.StatusFailure)

	// Simulate the re-register having completed during the backoff: a fresh
	// body is live under a different run ID (the slot is bound before the body
	// fires, so slot presence means "body in flight").
	seedSlot(eng, spec.ID, "live-restarted-run")

	// Synchronous: sleeps daemonBackoffInit, then must skip on the live slot.
	eng.onDaemonRunFinished(spec, runID)

	if got := exec.runs.Load(); got != 0 {
		t.Fatalf("executed %d daemon bodies, want 0 — a live run slot must suppress the backoff restart", got)
	}
	slot := eng.slots.runID(spec.ID)
	if slot != "live-restarted-run" {
		t.Fatalf("daemon slot = %q, want the concurrent starter's %q left untouched", slot, "live-restarted-run")
	}
}

// TestOnDaemonRunFinished_BackoffRestart_SkipsWhenUnregisteredDuringBackoff —
// the stillRegistered check at the top of onDaemonRunFinished predates the backoff sleep, so an
// Unregister during the sleep must be re-checked before restarting — an
// unregistered daemon must not be resurrected.
func TestOnDaemonRunFinished_BackoffRestart_SkipsWhenUnregisteredDuringBackoff(t *testing.T) {
	eng, spec, exec := daemonRaceEnv(t, "always")

	runID := quickFailureRun(t, eng, spec.ID, registry.StatusFailure)

	// Unregister mid-backoff: the delay is well past the (instant, in-memory)
	// top-of-function checks and well inside the 1s backoff sleep.
	go func() {
		time.Sleep(200 * time.Millisecond)
		eng.daemonMu.Lock()
		delete(eng.daemonSpecs, spec.ID)
		eng.daemonMu.Unlock()
	}()

	eng.onDaemonRunFinished(spec, runID)

	if got := exec.runs.Load(); got != 0 {
		t.Fatalf("executed %d daemon bodies, want 0 — an unregister during the backoff must not resurrect the daemon", got)
	}
}

// blockingDaemonExec blocks each body until its context is cancelled.
type blockingDaemonExec struct {
	started   chan string
	cancelled chan string
}

func (b *blockingDaemonExec) Execute(ctx context.Context, _ *task.Spec, opts pkgruntime.RunOptions) (*pkgruntime.RunResult, error) {
	b.started <- opts.RunID
	<-ctx.Done()
	b.cancelled <- opts.RunID
	return &pkgruntime.RunResult{RunID: opts.RunID, Error: ctx.Err()}, nil
}

// TestStartDaemon_EvictedBeforeRunRegistered_KillsBody: an Unregister that
// lands after the slot is bound but before fireAsync registers the run cannot
// KillRun it, so launchDaemon must kill the body itself once it notices the
// eviction.
func TestStartDaemon_EvictedBeforeRunRegistered_KillsBody(t *testing.T) {
	eng, spec, _ := daemonRaceEnv(t, "always")
	exec := &blockingDaemonExec{started: make(chan string, 4), cancelled: make(chan string, 4)}
	eng.RegisterExecutor(task.RuntimeDocker, exec)

	// The fire guard runs inside fireAsync before the run is registered.
	var once sync.Once
	eng.SetFireGuard(func(taskID string) error {
		once.Do(func() {
			eng.daemonMu.Lock()
			delete(eng.daemonSpecs, spec.ID)
			eng.daemonMu.Unlock()
			if eng.slots.evict(spec.ID) == "" {
				t.Error("slot was not bound at fire time")
			}
		})
		return nil
	})

	eng.startDaemon(spec)

	select {
	case <-exec.started:
	case <-time.After(5 * time.Second):
		t.Fatal("body never started")
	}
	select {
	case <-exec.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("body launched after eviction was never killed")
	}
}
