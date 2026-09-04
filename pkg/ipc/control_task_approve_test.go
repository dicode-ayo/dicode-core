package ipc

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"
)

func newApproveTestServer(approver func(string) error) *ControlServer {
	return &ControlServer{
		taskApprover: approver,
		log:          zap.NewNop(),
	}
}

// TestTaskApprove_EnabledDefaultsTrueWithNoLookup guards the historical
// wording ("triggers armed") for callers that never wire SetTaskEnabled —
// today only tests, since the daemon always wires it, but the default must
// stay accurate should that ever change.
func TestTaskApprove_EnabledDefaultsTrueWithNoLookup(t *testing.T) {
	cs := newApproveTestServer(func(string) error { return nil })

	res, err := cs.dispatch(context.Background(), Request{ID: "1", Method: "cli.task.approve", TaskID: "repo/deploy"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	out := res.(TaskApproveResult)
	if !out.Enabled {
		t.Fatalf("result = %+v, want Enabled=true with no taskEnabled lookup wired", out)
	}
}

// TestTaskApprove_ReportsDisabledFromLookup is the regression for #822:
// cli.task.approve must report Enabled=false for a disabled task so the CLI
// can avoid claiming "triggers armed".
func TestTaskApprove_ReportsDisabledFromLookup(t *testing.T) {
	cs := newApproveTestServer(func(string) error { return nil })
	cs.SetTaskEnabled(func(id string) (bool, bool) {
		if id != "repo/deploy" {
			t.Fatalf("taskEnabled called with unexpected id %q", id)
		}
		return false, true
	})

	res, err := cs.dispatch(context.Background(), Request{ID: "1", Method: "cli.task.approve", TaskID: "repo/deploy"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	out := res.(TaskApproveResult)
	if out.Enabled {
		t.Fatalf("result = %+v, want Enabled=false", out)
	}
}

// TestTaskApprove_LookupMissDefaultsEnabledTrue covers a taskEnabled lookup
// that reports "unknown" (ok=false) — must not be mistaken for a confirmed
// disabled task.
func TestTaskApprove_LookupMissDefaultsEnabledTrue(t *testing.T) {
	cs := newApproveTestServer(func(string) error { return nil })
	cs.SetTaskEnabled(func(string) (bool, bool) { return false, false })

	res, err := cs.dispatch(context.Background(), Request{ID: "1", Method: "cli.task.approve", TaskID: "repo/deploy"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	out := res.(TaskApproveResult)
	if !out.Enabled {
		t.Fatalf("result = %+v, want Enabled=true on a lookup miss", out)
	}
}

func TestTaskApprove_Success(t *testing.T) {
	var approved string
	cs := newApproveTestServer(func(id string) error {
		approved = id
		return nil
	})

	res, err := cs.dispatch(context.Background(), Request{ID: "1", Method: "cli.task.approve", TaskID: "repo/deploy"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	out, ok := res.(TaskApproveResult)
	if !ok {
		t.Fatalf("result type %T", res)
	}
	if !out.Approved || out.TaskID != "repo/deploy" {
		t.Fatalf("result = %+v", out)
	}
	if approved != "repo/deploy" {
		t.Fatalf("gate approver called with %q", approved)
	}
}

func TestTaskApprove_NotPendingErrorPropagates(t *testing.T) {
	gateErr := errors.New(`task "repo/deploy" is not pending approval`)
	cs := newApproveTestServer(func(string) error { return gateErr })

	_, err := cs.dispatch(context.Background(), Request{ID: "1", Method: "cli.task.approve", TaskID: "repo/deploy"})
	if !errors.Is(err, gateErr) {
		t.Fatalf("err = %v, want gate error", err)
	}
}

func TestTaskApprove_RequiresTaskID(t *testing.T) {
	called := false
	cs := newApproveTestServer(func(string) error { called = true; return nil })

	if _, err := cs.dispatch(context.Background(), Request{ID: "1", Method: "cli.task.approve"}); err == nil {
		t.Fatal("expected error without taskID")
	}
	if called {
		t.Fatal("approver must not be called without a task id")
	}
}

func TestTaskApprove_NotConfigured(t *testing.T) {
	cs := newApproveTestServer(nil)
	if _, err := cs.dispatch(context.Background(), Request{ID: "1", Method: "cli.task.approve", TaskID: "x"}); err == nil {
		t.Fatal("expected error when approval gate is not wired")
	}
}
