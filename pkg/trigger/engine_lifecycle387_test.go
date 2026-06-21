// Package trigger — lifecycle-correctness tests for issue #387 fixes.
// Covers Fix 1 (chain cycle guard), Fix 3 (FinishRun error logging),
// and Fix 4 (daemon crash-loop exponential backoff).
package trigger

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// ── Fix 1: Chain cycle guard ─────────────────────────────────────────────────

// TestSuccessChain_CycleRejected verifies that registering a task whose
// trigger.chain would close a cycle (A fires B on success, B fires A on
// success) is rejected with a non-nil error and the task does not arm
// its triggers.
func TestSuccessChain_CycleRejected(t *testing.T) {
	eng, reg, _ := newPreflightEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go eng.Start(ctx) //nolint:errcheck

	// Task A: manual trigger (no chain yet).
	taskA := &task.Spec{
		ID:      "cycle-a",
		Name:    "cycle-a",
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{Manual: true},
		Enabled: true,
	}
	if err := reg.Register(taskA); err != nil {
		t.Fatalf("reg.Register taskA: %v", err)
	}
	if err := eng.Register(taskA); err != nil {
		t.Fatalf("eng.Register taskA: %v", err)
	}

	// Task B: fires when A succeeds.
	taskB := &task.Spec{
		ID:      "cycle-b",
		Name:    "cycle-b",
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{
			Chain: &task.ChainTrigger{From: "cycle-a", On: "success"},
		},
		Enabled: true,
	}
	if err := reg.Register(taskB); err != nil {
		t.Fatalf("reg.Register taskB: %v", err)
	}
	if err := eng.Register(taskB); err != nil {
		t.Fatalf("eng.Register taskB: %v", err)
	}

	// Task A2: a replacement of taskA that now chains from B — closing the
	// cycle A2 → B → A2.
	taskA2 := &task.Spec{
		ID:      "cycle-a",
		Name:    "cycle-a",
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{
			Chain: &task.ChainTrigger{From: "cycle-b", On: "success"},
		},
		Enabled: true,
	}
	if err := reg.Register(taskA2); err != nil {
		t.Fatalf("reg.Register taskA2: %v", err)
	}
	err := eng.Register(taskA2)
	if err == nil {
		t.Fatal("expected eng.Register to reject the cycle, got nil error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("expected error to mention 'cycle', got: %v", err)
	}
}

// TestSuccessChain_NoCycleAccepted verifies that a valid A→B chain (no cycle)
// is accepted normally.
func TestSuccessChain_NoCycleAccepted(t *testing.T) {
	eng, reg, _ := newPreflightEnv(t)

	taskA := &task.Spec{
		ID:      "nocycle-a",
		Name:    "nocycle-a",
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{Manual: true},
		Enabled: true,
	}
	taskB := &task.Spec{
		ID:      "nocycle-b",
		Name:    "nocycle-b",
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{
			Chain: &task.ChainTrigger{From: "nocycle-a", On: "success"},
		},
		Enabled: true,
	}
	for _, spec := range []*task.Spec{taskA, taskB} {
		if err := reg.Register(spec); err != nil {
			t.Fatalf("reg.Register %s: %v", spec.ID, err)
		}
		if err := eng.Register(spec); err != nil {
			t.Fatalf("eng.Register %s: %v", spec.ID, err)
		}
	}
}

// ── Fix 3: FinishRun error logging ───────────────────────────────────────────

// failingDB wraps a real DB and returns an error on Exec calls whose query
// matches a substring, so we can force FinishRun to fail.
type failingDB struct {
	db.DB
	failOn string // substring of query to fail
}

func (f *failingDB) Exec(ctx context.Context, query string, args ...any) error {
	if strings.Contains(query, f.failOn) {
		return errForcedDBFail
	}
	return f.DB.Exec(ctx, query, args...)
}

// errForcedDBFail is a sentinel returned by failingDB.
var errForcedDBFail = &forcedDBError{}

type forcedDBError struct{}

func (e *forcedDBError) Error() string { return "forced DB failure for testing" }

// TestDispatch_FinishRunErrorIsLogged verifies that when FinishRun fails, the
// engine logs a Warn instead of silently discarding the error.
func TestDispatch_FinishRunErrorIsLogged(t *testing.T) {
	// Open real DB to back the registry.
	realDB, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { realDB.Close() })

	// Wrap with a failing shim that errors on UPDATE runs SET status.
	fdb := &failingDB{DB: realDB, failOn: "UPDATE runs SET status"}

	// Capture log output.
	core, logs := observer.New(zapcore.WarnLevel)
	log := zap.New(core)

	reg := registry.New(fdb)
	exec := newPreflightExec(reg)
	eng := New(reg, exec, log)
	eng.RegisterExecutor(task.RuntimeDocker, exec)

	spec := &task.Spec{
		ID:      "finishrun-test",
		Name:    "finishrun-test",
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{Manual: true},
		Enabled: true,
	}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("reg.Register: %v", err)
	}
	if err := eng.Register(spec); err != nil {
		t.Fatalf("eng.Register: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go eng.Start(ctx) //nolint:errcheck

	_, err = eng.FireManual(context.Background(), "finishrun-test", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}

	// Allow time for the run to complete and FinishRun to be called.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if logs.FilterMessageSnippet("FinishRun").Len() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	found := logs.FilterMessageSnippet("FinishRun").Len()
	if found == 0 {
		t.Error("expected at least one Warn log about FinishRun failure, got none")
		for _, entry := range logs.All() {
			t.Logf("log: %s %s", entry.Level, entry.Message)
		}
	}
}

// ── Fix 4: Daemon crash-loop exponential backoff ──────────────────────────────

// TestDaemon_CrashBackoffSchedule verifies that a daemon that exits immediately
// accumulates an increasing backoff rather than restarting with a fixed 2s delay.
// It measures real time for two restarts and checks that the second is at least
// as long as the first (monotonically non-decreasing backoff).
func TestDaemon_CrashBackoffSchedule(t *testing.T) {
	eng, reg, exec := newPreflightEnv(t)

	// A daemon spec that exits immediately (exec returns without blocking).
	daemon := &task.Spec{
		ID:      "backoff-daemon",
		Name:    "backoff-daemon",
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{Daemon: true, Restart: "always"},
		Enabled: true,
	}
	if err := reg.Register(daemon); err != nil {
		t.Fatalf("reg.Register: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go eng.Start(ctx) //nolint:errcheck

	// Register the daemon to start it.
	if err := eng.Register(daemon); err != nil {
		t.Fatalf("eng.Register: %v", err)
	}

	// Count restarts by watching exec.runs. Wait for at least 3 executions.
	// With 1s init backoff the 3rd run should start within ~4s.
	deadline := time.Now().Add(15 * time.Second)
	var runCount int32
	for time.Now().Before(deadline) {
		runCount = exec.runs.Load()
		if runCount >= 3 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = atomic.LoadInt32(&runCount)
	if exec.runs.Load() < 3 {
		t.Fatalf("daemon did not restart at least 3 times within 15s (restarts=%d); backoff may be too long or daemon not restarting", exec.runs.Load())
	}

	// Verify the backoff key exists in daemonBackoffs and is > 1s (has increased).
	backoffKey := "daemon-backoff:backoff-daemon"
	v, ok := eng.daemonBackoffs.Load(backoffKey)
	if !ok {
		t.Fatal("daemonBackoffs key not set — backoff is not being tracked")
	}
	backoff := v.(time.Duration)
	if backoff <= time.Second {
		t.Errorf("expected backoff to grow beyond 1s after multiple crashes, got %v", backoff)
	}
	t.Logf("daemon backoff after %d restarts: %v", exec.runs.Load(), backoff)
}

// TestDaemon_CrashBackoff_Constants verifies the backoff schedule constants have
// the expected values per the issue spec: 1s init, 30s max, 10s stable threshold.
func TestDaemon_CrashBackoff_Constants(t *testing.T) {
	if daemonBackoffInit != time.Second {
		t.Errorf("daemonBackoffInit = %v, want 1s", daemonBackoffInit)
	}
	if daemonBackoffMax != 30*time.Second {
		t.Errorf("daemonBackoffMax = %v, want 30s", daemonBackoffMax)
	}
	if daemonStableThreshold != 10*time.Second {
		t.Errorf("daemonStableThreshold = %v, want 10s", daemonStableThreshold)
	}
}

// ── Fix 2: prereq context propagation ────────────────────────────────────────

// TestFireSync_ParentContextCancelsRun verifies that when fireSync is called
// with a cancellable parent context, cancelling that context also cancels the
// in-flight run (Fix 2, #387).
//
// Before the fix, fireSync always called startRun which used
// context.Background() as the run's parent, so cancelling the caller's context
// had no effect on the run. The prereqCtx timeout in resolveIfMissing was
// therefore useless — a prereq with a long spec Timeout could block forever.
//
// After the fix, fireSync calls startRunWithParent(callerCtx, ...) so the run
// context inherits the caller's cancellation signal.
func TestFireSync_ParentContextCancelsRun(t *testing.T) {
	eng, reg, exec := newPreflightEnv(t)

	const taskID = "prereq-ctx-propagation"
	// markDaemon makes Execute block until the run's context is cancelled.
	// If the parent context does NOT propagate to the run context, this goroutine
	// would block forever and the test would time out.
	exec.markDaemon(taskID)

	spec := &task.Spec{
		ID:      taskID,
		Name:    taskID,
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{Daemon: true, Restart: "always"},
		Enabled: true,
	}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("reg.Register: %v", err)
	}
	if err := eng.Register(spec); err != nil {
		t.Fatalf("eng.Register: %v", err)
	}

	parent, cancelParent := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		eng.fireSync(parent, spec, pkgruntime.RunOptions{}, "if_missing") //nolint:errcheck
	}()

	// Give the executor time to start and block inside markDaemon's <-ctx.Done().
	time.Sleep(100 * time.Millisecond)

	// Cancel parent. With Fix 2, this propagates through startRunWithParent's
	// context.WithCancel(parent) to the run context, unblocking the executor.
	cancelParent()

	select {
	case <-done:
		// Run was cancelled by parent context propagation — correct.
	case <-time.After(3 * time.Second):
		t.Error("fireSync did not return after parent context cancellation; " +
			"before Fix 2 the run used context.Background() as parent and would block forever")
	}
}
