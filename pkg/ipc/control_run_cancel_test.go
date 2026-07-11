package ipc

// cli.run.cancel backs the CLI follow loop's Ctrl+C-during-a-turn behavior:
// interrupting a blocking cli.run.wait cancels the run on the daemon instead
// of merely exiting the CLI. This pins the dispatch → engine.KillRun wiring.

import (
	"context"
	"testing"

	"go.uber.org/zap"
)

// killRecordingEngine records the run id passed to KillRun and returns a
// scripted result, mirroring how *trigger.Engine reports whether the run was
// still cancellable.
type killRecordingEngine struct {
	mockEngine
	killedID string
	killOK   bool
}

func (e *killRecordingEngine) KillRun(runID string) bool {
	e.killedID = runID
	return e.killOK
}

func TestCLIRunCancel_DispatchesToEngineKillRun(t *testing.T) {
	eng := &killRecordingEngine{killOK: true}
	cs := &ControlServer{engine: eng, log: zap.NewNop()}

	res, err := cs.dispatch(context.Background(), Request{ID: "1", Method: "cli.run.cancel", RunID: "run-123"})
	if err != nil {
		t.Fatalf("dispatch cli.run.cancel: %v", err)
	}

	if eng.killedID != "run-123" {
		t.Fatalf("engine.KillRun called with runID %q, want %q", eng.killedID, "run-123")
	}
	out, ok := res.(RunCancelResult)
	if !ok {
		t.Fatalf("result type = %T, want RunCancelResult", res)
	}
	if !out.Killed {
		t.Fatalf("result.Killed = false, want true")
	}
}

func TestCLIRunCancel_MissingRunID(t *testing.T) {
	eng := &killRecordingEngine{killOK: true}
	cs := &ControlServer{engine: eng, log: zap.NewNop()}

	if _, err := cs.dispatch(context.Background(), Request{ID: "1", Method: "cli.run.cancel"}); err == nil {
		t.Fatal("dispatch cli.run.cancel with empty runID: want error, got nil")
	}
	if eng.killedID != "" {
		t.Fatalf("engine.KillRun should not have been called, got runID %q", eng.killedID)
	}
}

func TestCLIRunCancel_NotCancellable(t *testing.T) {
	eng := &killRecordingEngine{killOK: false}
	cs := &ControlServer{engine: eng, log: zap.NewNop()}

	res, err := cs.dispatch(context.Background(), Request{ID: "1", Method: "cli.run.cancel", RunID: "gone"})
	if err != nil {
		t.Fatalf("dispatch cli.run.cancel: %v", err)
	}
	out := res.(RunCancelResult)
	if out.Killed {
		t.Fatal("result.Killed = true, want false for a non-cancellable run")
	}
}
