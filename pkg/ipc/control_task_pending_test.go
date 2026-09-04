package ipc

import (
	"context"
	"testing"

	"go.uber.org/zap"
)

func newPendingTestServer(lister func() []PendingTask) *ControlServer {
	return &ControlServer{
		pendingApprovals: lister,
		log:              zap.NewNop(),
	}
}

// boolPtr returns a pointer to b, for populating PendingTask.Enabled /
// TaskApproveResult.Enabled test fixtures (a *bool, per PR #830 CodeRabbit
// finding 3, so an absent field on the wire decodes as nil rather than the
// ambiguous zero value false).
func boolPtr(b bool) *bool { return &b }

func TestTaskPending_SeveralPending(t *testing.T) {
	longHash := "0123456789abcdef0123456789abcdef" // > 12 chars → shortened
	cs := newPendingTestServer(func() []PendingTask {
		return []PendingTask{
			{TaskID: "repo/deploy", Hash: longHash},
			{TaskID: "repo/cleanup", Hash: "abc"}, // <= 12 chars → verbatim
		}
	})

	res, err := cs.dispatch(context.Background(), Request{ID: "1", Method: "cli.task.pending"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	out, ok := res.([]PendingTask)
	if !ok {
		t.Fatalf("result type %T", res)
	}
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2 (%+v)", len(out), out)
	}
	if out[0].TaskID != "repo/deploy" || out[0].Hash != "0123456789ab"+"…" {
		t.Fatalf("row 0 = %+v, want shortened hash", out[0])
	}
	if out[1].TaskID != "repo/cleanup" || out[1].Hash != "abc" {
		t.Fatalf("row 1 = %+v, want short hash verbatim", out[1])
	}
}

// TestTaskPending_SurfacesDisabledFlag is the regression for #822: a
// disabled-but-pending task must be listed with Enabled=false so a headless
// operator doesn't mistake it for a hold blocking something that would
// otherwise run.
func TestTaskPending_SurfacesDisabledFlag(t *testing.T) {
	cs := newPendingTestServer(func() []PendingTask {
		return []PendingTask{
			{TaskID: "repo/deploy", Hash: "abc", Enabled: boolPtr(true)},
			{TaskID: "repo/off", Hash: "def", Enabled: boolPtr(false)},
		}
	})

	res, err := cs.dispatch(context.Background(), Request{ID: "1", Method: "cli.task.pending"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	out, ok := res.([]PendingTask)
	if !ok {
		t.Fatalf("result type %T", res)
	}
	if len(out) != 2 || out[0].Enabled == nil || !*out[0].Enabled || out[1].Enabled == nil || *out[1].Enabled {
		t.Fatalf("out = %+v, want [Enabled=&true, Enabled=&false]", out)
	}
}

// TestTaskPending_EnabledNilWhenUnset covers the wire-compatibility contract
// (PR #830 CodeRabbit finding 3): a nil Enabled (as an older daemon's
// pendingApprovals-equivalent response would produce, or simply an unset
// field in a hand-built fixture) must decode/relay as nil, distinguishable
// from an explicit false, rather than defaulting to a zero value.
func TestTaskPending_EnabledNilWhenUnset(t *testing.T) {
	cs := newPendingTestServer(func() []PendingTask {
		return []PendingTask{{TaskID: "repo/deploy", Hash: "abc"}}
	})

	res, err := cs.dispatch(context.Background(), Request{ID: "1", Method: "cli.task.pending"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	out, ok := res.([]PendingTask)
	if !ok {
		t.Fatalf("result type %T", res)
	}
	if len(out) != 1 || out[0].Enabled != nil {
		t.Fatalf("out = %+v, want Enabled=nil", out)
	}
}

func TestTaskPending_NonePending(t *testing.T) {
	cs := newPendingTestServer(func() []PendingTask { return nil })

	res, err := cs.dispatch(context.Background(), Request{ID: "1", Method: "cli.task.pending"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	out, ok := res.([]PendingTask)
	if !ok {
		t.Fatalf("result type %T", res)
	}
	if out == nil || len(out) != 0 {
		t.Fatalf("out = %#v, want non-nil empty slice", out)
	}
}

func TestTaskPending_GateNotConfigured(t *testing.T) {
	cs := newPendingTestServer(nil)

	res, err := cs.dispatch(context.Background(), Request{ID: "1", Method: "cli.task.pending"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	out, ok := res.([]PendingTask)
	if !ok {
		t.Fatalf("result type %T", res)
	}
	if len(out) != 0 {
		t.Fatalf("out = %+v, want empty list when gate is nil", out)
	}
}

func TestTaskPending_AnnotatesPendingTaskIDs(t *testing.T) {
	cs := newPendingTestServer(func() []PendingTask {
		return []PendingTask{{TaskID: "repo/deploy", Hash: "deadbeef"}}
	})
	set := cs.pendingTaskIDs()
	if _, ok := set["repo/deploy"]; !ok {
		t.Fatalf("pendingTaskIDs = %v, want repo/deploy present", set)
	}
	if _, ok := set["repo/other"]; ok {
		t.Fatal("pendingTaskIDs must not contain unlisted task")
	}

	if got := newPendingTestServer(nil).pendingTaskIDs(); got != nil {
		t.Fatalf("nil gate pendingTaskIDs = %v, want nil", got)
	}
}
