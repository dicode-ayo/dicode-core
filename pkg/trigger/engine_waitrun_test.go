package trigger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
)

// waitForTerminal polls the registry until the run leaves the 'running'
// state or the deadline elapses. Returns the final run record.
func waitForTerminal(t *testing.T, e *Engine, runID string, timeout time.Duration) *registry.Run {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		run, err := e.registry.GetRun(context.Background(), runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if run.Status != registry.StatusRunning {
			return run
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("run %s did not finish within %v", runID, timeout)
	return nil
}

// TestEngine_WaitRun_ReturnsSuccessResult covers the happy path: fire a
// short task, call WaitRun, expect status=success with the task's return
// value decoded back into the ipc.RunResult.
func TestEngine_WaitRun_ReturnsSuccessResult(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)
	spec := writeTask(t, dir, "wait-success",
		`export default async function main() { return { ok: true, n: 42 } }`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(spec)

	runID, err := e.engine.FireManual(context.Background(), "wait-success", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := e.engine.WaitRun(ctx, runID)
	if err != nil {
		t.Fatalf("WaitRun: %v", err)
	}
	if result.RunID != runID {
		t.Errorf("result.RunID = %q, want %q", result.RunID, runID)
	}
	if result.Status != registry.StatusSuccess {
		t.Errorf("status = %q, want success", result.Status)
	}
	// ReturnValue is unmarshalled into interface{} — JSON object becomes map[string]any.
	m, ok := result.ReturnValue.(map[string]interface{})
	if !ok {
		t.Fatalf("return value is %T, want map; value=%v", result.ReturnValue, result.ReturnValue)
	}
	if m["ok"] != true {
		t.Errorf("return.ok = %v, want true", m["ok"])
	}
	if m["n"].(float64) != 42 {
		t.Errorf("return.n = %v, want 42", m["n"])
	}
}

// TestEngine_WaitRun_AlreadyFinished verifies the fast path: when the run
// completed before WaitRun was called, it should skip the channel wait and
// return the persisted record immediately.
func TestEngine_WaitRun_AlreadyFinished(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)
	spec := writeTask(t, dir, "wait-finished",
		`export default async function main() { return "done" }`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(spec)

	runID, err := e.engine.FireManual(context.Background(), "wait-finished", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	waitForTerminal(t, e.engine, runID, 30*time.Second)

	// Now WaitRun should return immediately without blocking.
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := e.engine.WaitRun(ctx, runID)
	if err != nil {
		t.Fatalf("WaitRun: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("WaitRun on finished run took %v, want <500ms", elapsed)
	}
	if result.Status != registry.StatusSuccess {
		t.Errorf("status = %q, want success", result.Status)
	}
}

// TestEngine_WaitRun_NotFound verifies that an unknown run ID returns
// ErrRunNotFound rather than hanging or nil.
func TestEngine_WaitRun_NotFound(t *testing.T) {
	e := newTestEnv(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := e.engine.WaitRun(ctx, "00000000-0000-0000-0000-000000000000")
	if err == nil {
		t.Fatal("expected error for unknown run, got nil")
	}
	if !errors.Is(err, ErrRunNotFound) {
		t.Errorf("err = %v, want ErrRunNotFound", err)
	}
}

// TestEngine_WaitRun_ContextCanceled verifies that cancelling the waiter's
// context unblocks WaitRun with ctx.Err() while the run is still live.
func TestEngine_WaitRun_ContextCanceled(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)
	// Long-running task — sleeps 10s so we have time to cancel.
	spec := writeTask(t, dir, "wait-slow",
		`export default async function main() { await new Promise(r => setTimeout(r, 10_000)); return "ok" }`,
		task.TriggerConfig{Manual: true})
	_ = e.reg.Register(spec)

	runID, err := e.engine.FireManual(context.Background(), "wait-slow", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err = e.engine.WaitRun(ctx, runID)
	if err == nil {
		t.Fatal("expected error for canceled context, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.DeadlineExceeded or Canceled", err)
	}

	// Clean up: kill the still-running task so the test teardown can close
	// the DB without a live runner holding cursors.
	e.engine.KillRun(runID)
	waitForTerminal(t, e.engine, runID, 30*time.Second)
}

// A run that suspends is not terminal: WaitRun must keep blocking and follow
// the resume chain, returning the continuation run's terminal result rather
// than an intermediate {status: suspended}. Drives the suspend/resume state
// transitions at the registry level so no live runtime is needed.
func TestEngine_WaitRun_FollowsResumeChain(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	const origID = "orig-run"
	if _, err := e.reg.StartRunWithID(ctx, origID, "wizard", "", string(registry.TriggerManual), registry.RunKindTask); err != nil {
		t.Fatalf("StartRunWithID: %v", err)
	}
	if _, err := e.reg.SuspendRun(ctx, origID, []byte(`{"step":1}`), nil, "tok", time.Now().UnixMilli(), 0, nil); err != nil {
		t.Fatalf("SuspendRun: %v", err)
	}

	type waitOut struct {
		status string
		runID  string
		ret    interface{}
		err    error
	}
	done := make(chan waitOut, 1)
	go func() {
		wctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		r, err := e.engine.WaitRun(wctx, origID)
		done <- waitOut{status: r.Status, runID: r.RunID, ret: r.ReturnValue, err: err}
	}()

	// WaitRun must NOT return while the run is still suspended (the old bug
	// returned {status: suspended} immediately).
	select {
	case out := <-done:
		t.Fatalf("WaitRun returned while suspended: status=%q err=%v", out.status, out.err)
	case <-time.After(600 * time.Millisecond):
	}

	// Resume: mark the original resumed and spawn the continuation run that
	// carries the real result.
	if err := e.reg.MarkRunResumed(ctx, origID); err != nil {
		t.Fatalf("MarkRunResumed: %v", err)
	}
	const contID = "cont-run"
	if _, err := e.reg.StartRunWithID(ctx, contID, "wizard", origID, string(registry.TriggerResume), registry.RunKindTask); err != nil {
		t.Fatalf("StartRunWithID continuation: %v", err)
	}
	if err := e.reg.FinishRunWithResult(ctx, contID, registry.StatusSuccess, `{"answer":42}`, "", ""); err != nil {
		t.Fatalf("FinishRunWithResult: %v", err)
	}

	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("WaitRun err: %v", out.err)
		}
		if out.status != registry.StatusSuccess {
			t.Errorf("status = %q, want success", out.status)
		}
		if out.runID != contID {
			t.Errorf("runID = %q, want continuation %q", out.runID, contID)
		}
		m, ok := out.ret.(map[string]interface{})
		if !ok {
			t.Fatalf("return value %T, want map", out.ret)
		}
		if m["answer"].(float64) != 42 {
			t.Errorf("answer = %v, want 42", m["answer"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitRun did not return after resume+continuation finished")
	}
}
