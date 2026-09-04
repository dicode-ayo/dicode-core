package ipc

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"
)

func newApproveTestServer(approver func(string) (bool, error)) *ControlServer {
	return &ControlServer{
		taskApprover: approver,
		log:          zap.NewNop(),
	}
}

// TestTaskApprove_ReportsEnabledFromApprover is the regression for #822 and
// its TOCTOU follow-up (PR #830 CodeRabbit finding 1): the approver
// (approvalGate.ApproveReporting in production) returns the enabled flag in
// the SAME call that performs the approval, and handleTaskApprove must relay
// that value directly rather than looking it up separately afterward.
func TestTaskApprove_ReportsEnabledFromApprover(t *testing.T) {
	cs := newApproveTestServer(func(string) (bool, error) { return true, nil })

	res, err := cs.dispatch(context.Background(), Request{ID: "1", Method: "cli.task.approve", TaskID: "repo/deploy"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	out := res.(TaskApproveResult)
	if out.Enabled == nil || !*out.Enabled {
		t.Fatalf("result = %+v, want Enabled=&true", out)
	}
}

// TestTaskApprove_ReportsDisabledFromApprover is the regression for #822:
// cli.task.approve must report Enabled=false for a disabled task so the CLI
// can avoid claiming "triggers armed".
func TestTaskApprove_ReportsDisabledFromApprover(t *testing.T) {
	cs := newApproveTestServer(func(id string) (bool, error) {
		if id != "repo/deploy" {
			t.Fatalf("approver called with unexpected id %q", id)
		}
		return false, nil
	})

	res, err := cs.dispatch(context.Background(), Request{ID: "1", Method: "cli.task.approve", TaskID: "repo/deploy"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	out := res.(TaskApproveResult)
	if out.Enabled == nil || *out.Enabled {
		t.Fatalf("result = %+v, want Enabled=&false", out)
	}
}

func TestTaskApprove_Success(t *testing.T) {
	var approved string
	cs := newApproveTestServer(func(id string) (bool, error) {
		approved = id
		return true, nil
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
	cs := newApproveTestServer(func(string) (bool, error) { return false, gateErr })

	_, err := cs.dispatch(context.Background(), Request{ID: "1", Method: "cli.task.approve", TaskID: "repo/deploy"})
	if !errors.Is(err, gateErr) {
		t.Fatalf("err = %v, want gate error", err)
	}
}

func TestTaskApprove_RequiresTaskID(t *testing.T) {
	called := false
	cs := newApproveTestServer(func(string) (bool, error) { called = true; return true, nil })

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
