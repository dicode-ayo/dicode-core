package trigger

// Shutdown-drain regression tests (issue #520).
//
// On shutdown the engine killed in-flight daemon runs but returned before their
// per-run goroutines finished FinishRun/status writes. The daemon then closed
// the DB (deferred after the errgroup unblocks), so finalization hit
// `sql: database is closed`. Start now drains runWG before returning, bounded by
// drainGrace so a wedged run cannot hang shutdown forever.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// drainExec is a daemon body whose termination behaviour the test controls.
// It signals when the body begins so the test can find the run's ID before
// triggering shutdown.
type drainExec struct {
	started     chan struct{}
	startedOnce sync.Once

	// onCancelDelay, when > 0, is slept after the run context is cancelled and
	// before Execute returns — widening the window in which the run's
	// finalization runs, so a Start that failed to drain would return with the
	// run still non-terminal.
	onCancelDelay time.Duration

	// block, when non-nil, is waited on INSTEAD of ctx — a wedged run that
	// ignores KillRun. The test releases it during teardown.
	block chan struct{}
}

func (d *drainExec) Execute(ctx context.Context, _ *task.Spec, opts pkgruntime.RunOptions) (*pkgruntime.RunResult, error) {
	d.startedOnce.Do(func() { close(d.started) })
	if d.block != nil {
		<-d.block
		return &pkgruntime.RunResult{RunID: opts.RunID}, nil
	}
	<-ctx.Done()
	if d.onCancelDelay > 0 {
		time.Sleep(d.onCancelDelay)
	}
	return &pkgruntime.RunResult{RunID: opts.RunID}, nil
}

func newDrainEnv(t *testing.T, exec *drainExec) (*Engine, *registry.Registry) {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)
	eng := New(reg, exec, zap.NewNop())
	eng.RegisterExecutor(task.RuntimeDocker, exec)
	return eng, reg
}

func drainDaemonSpec() *task.Spec {
	return &task.Spec{
		ID:      "drain-daemon",
		Name:    "drain-daemon",
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{Daemon: true, Restart: "never"},
		Enabled: true,
	}
}

// runningDaemonRunID waits until the daemon body has begun and returns its run
// ID from the engine's daemonRuns slot.
func runningDaemonRunID(t *testing.T, eng *Engine, exec *drainExec, taskID string) string {
	t.Helper()
	select {
	case <-exec.started:
	case <-time.After(2 * time.Second):
		t.Fatal("daemon body never started")
	}
	var runID string
	waitUntil(t, 2*time.Second, func() bool {
		eng.daemonMu.Lock()
		runID = eng.daemonRuns[taskID]
		eng.daemonMu.Unlock()
		return runID != ""
	}, "daemon run slot never populated")
	return runID
}

// TestShutdownDrainsInFlightRunFinalization proves the DB-outlives-finalization
// invariant: a daemon run in flight when Start's ctx is cancelled must have its
// FinishRun/status write completed by the time Start returns. The executor
// sleeps after cancellation so a non-draining Start would observe the run still
// in `running`.
func TestShutdownDrainsInFlightRunFinalization(t *testing.T) {
	exec := &drainExec{started: make(chan struct{}), onCancelDelay: 150 * time.Millisecond}
	eng, reg := newDrainEnv(t, exec)

	spec := drainDaemonSpec()
	if err := reg.Register(spec); err != nil {
		t.Fatalf("reg.Register: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	startDone := make(chan error, 1)
	go func() { startDone <- eng.Start(ctx) }()

	runID := runningDaemonRunID(t, eng, exec, spec.ID)

	cancel()

	select {
	case <-startDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after shutdown")
	}

	// Start has returned; the run goroutine's finalization must already be done.
	run, err := reg.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun(%q): %v", runID, err)
	}
	if run.Status == registry.StatusRunning || run.Status == "" {
		t.Fatalf("run %q status = %q; Start returned before finalization drained", runID, run.Status)
	}
}

// TestShutdownDrainBoundedByGrace proves the drain cannot hang shutdown: a run
// that ignores its kill still lets Start return once drainGrace elapses.
func TestShutdownDrainBoundedByGrace(t *testing.T) {
	exec := &drainExec{started: make(chan struct{}), block: make(chan struct{})}
	eng, reg := newDrainEnv(t, exec)
	eng.drainGrace = 200 * time.Millisecond

	spec := drainDaemonSpec()
	if err := reg.Register(spec); err != nil {
		t.Fatalf("reg.Register: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	startDone := make(chan error, 1)
	go func() { startDone <- eng.Start(ctx) }()

	runningDaemonRunID(t, eng, exec, spec.ID)

	start := time.Now()
	cancel()

	select {
	case <-startDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Start hung past the drain grace on a wedged run")
	}
	elapsed := time.Since(start)

	if elapsed < eng.drainGrace {
		t.Errorf("Start returned after %v, before the %v grace — it did not wait for the drain", elapsed, eng.drainGrace)
	}

	// Release the wedged body and let its goroutine finalize against the still-
	// open DB before the deferred db.Close runs.
	close(exec.block)
	eng.runWG.Wait()
}
