package registry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
)

func newResumeTestReg(t *testing.T) *Registry {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return New(d)
}

// seedSuspendedRun creates a run and suspends it with the given token/deadline.
func seedSuspendedRun(t *testing.T, r *Registry, runID, token string, deadline int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := r.StartRunWithID(ctx, runID, "task-x", "", string(TriggerManual), RunKindTask); err != nil {
		t.Fatalf("StartRunWithID: %v", err)
	}
	if err := r.SuspendRun(ctx, runID, []byte(`{"step":1}`), []byte(`{"fields":[]}`), token, time.Now().UnixMilli(), deadline, nil); err != nil {
		t.Fatalf("SuspendRun: %v", err)
	}
}

func TestGetRunByResumeToken(t *testing.T) {
	r := newResumeTestReg(t)
	ctx := context.Background()
	seedSuspendedRun(t, r, "run-1", "tok-abc", 0)

	run, err := r.GetRunByResumeToken(ctx, "tok-abc")
	if err != nil {
		t.Fatalf("GetRunByResumeToken: %v", err)
	}
	if run.ID != "run-1" {
		t.Errorf("run ID = %q, want run-1", run.ID)
	}
	if run.Status != StatusSuspended {
		t.Errorf("status = %q, want suspended", run.Status)
	}
	if string(run.ResumeState) != `{"step":1}` {
		t.Errorf("resume state = %q", run.ResumeState)
	}

	if _, err := r.GetRunByResumeToken(ctx, "nope"); !errors.Is(err, ErrRunNotFound) {
		t.Errorf("unknown token err = %v, want ErrRunNotFound", err)
	}
	if _, err := r.GetRunByResumeToken(ctx, ""); !errors.Is(err, ErrRunNotFound) {
		t.Errorf("empty token err = %v, want ErrRunNotFound", err)
	}
}

func TestMarkRunResumed_SingleUse(t *testing.T) {
	r := newResumeTestReg(t)
	ctx := context.Background()
	seedSuspendedRun(t, r, "run-2", "tok-2", 0)

	if err := r.MarkRunResumed(ctx, "run-2"); err != nil {
		t.Fatalf("MarkRunResumed: %v", err)
	}
	run, err := r.GetRun(ctx, "run-2")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != StatusResumed {
		t.Errorf("status = %q, want resumed", run.Status)
	}
	if run.FinishedAt == nil {
		t.Error("resumed run should have finished_at set")
	}

	// Second attempt must fail — the token is single-use.
	if err := r.MarkRunResumed(ctx, "run-2"); !errors.Is(err, ErrRunNotSuspended) {
		t.Errorf("second MarkRunResumed err = %v, want ErrRunNotSuspended", err)
	}
}

func TestMarkRunResumed_NotFound(t *testing.T) {
	r := newResumeTestReg(t)
	if err := r.MarkRunResumed(context.Background(), "ghost"); !errors.Is(err, ErrRunNotFound) {
		t.Errorf("err = %v, want ErrRunNotFound", err)
	}
}

func TestSweepExpiredSuspensions(t *testing.T) {
	r := newResumeTestReg(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	seedSuspendedRun(t, r, "expired", "tok-e", now-1000)     // deadline in the past
	seedSuspendedRun(t, r, "future", "tok-f", now+1_000_000) // deadline in the future
	seedSuspendedRun(t, r, "nodeadline", "tok-n", 0)         // no deadline

	swept, err := r.SweepExpiredSuspensions(ctx, now)
	if err != nil {
		t.Fatalf("SweepExpiredSuspensions: %v", err)
	}
	if len(swept) != 1 || swept[0] != "expired" {
		t.Fatalf("swept = %v, want [expired]", swept)
	}

	expired, _ := r.GetRun(ctx, "expired")
	if expired.Status != StatusCancelled {
		t.Errorf("expired status = %q, want cancelled", expired.Status)
	}
	if expired.FailureReason != ReasonResumeTimeout {
		t.Errorf("expired fail_reason = %q, want %q", expired.FailureReason, ReasonResumeTimeout)
	}
	if expired.FinishedAt == nil {
		t.Error("swept run should have finished_at set")
	}

	// Untouched rows stay suspended.
	for _, id := range []string{"future", "nodeadline"} {
		run, _ := r.GetRun(ctx, id)
		if run.Status != StatusSuspended {
			t.Errorf("%s status = %q, want suspended", id, run.Status)
		}
	}
}
