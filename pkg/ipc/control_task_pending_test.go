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
