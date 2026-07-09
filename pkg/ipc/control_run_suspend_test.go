package ipc

import (
	"context"
	"testing"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	"go.uber.org/zap"
)

// waiterProbeEngine reports which waiter the control server chose. cli.run and
// cli.run.wait must use WaitRunSettled: WaitRun follows a suspended run's resume
// chain, so the CLI would block instead of rendering the wizard.
type waiterProbeEngine struct {
	mockEngine
	usedWaitRun    bool
	usedWaitSettle bool
}

func (r *waiterProbeEngine) WaitRun(_ context.Context, _ string) (RunResult, error) {
	r.usedWaitRun = true
	return RunResult{Status: "success"}, nil
}

func (r *waiterProbeEngine) WaitRunSettled(_ context.Context, runID string) (RunResult, error) {
	r.usedWaitSettle = true
	return RunResult{RunID: runID, Status: "suspended"}, nil
}

func TestCLIRun_UsesSettledWaiterSoSuspendedSurfaces(t *testing.T) {
	eng := &waiterProbeEngine{}
	cs := &ControlServer{engine: eng, log: zap.NewNop()}

	res, err := cs.dispatch(context.Background(), Request{ID: "1", Method: "cli.run", TaskID: "wizard"})
	if err != nil {
		t.Fatalf("dispatch cli.run: %v", err)
	}

	if eng.usedWaitRun {
		t.Error("cli.run used WaitRun; a suspending task would hang the CLI instead of showing its resume form")
	}
	if !eng.usedWaitSettle {
		t.Fatal("cli.run did not use WaitRunSettled")
	}
	rr, ok := res.(RunResult)
	if !ok {
		t.Fatalf("result type = %T, want RunResult", res)
	}
	if rr.Status != "suspended" {
		t.Errorf("status = %q, want the suspended status passed through to the CLI", rr.Status)
	}
}

func TestCLIRunWait_UsesSettledWaiter(t *testing.T) {
	eng := &waiterProbeEngine{}
	// handleRunWait consults the registry to short-circuit daemon runs.
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	cs := &ControlServer{engine: eng, reg: registry.New(d), log: zap.NewNop()}

	if _, err := cs.dispatch(context.Background(), Request{ID: "1", Method: "cli.run.wait", RunID: "run-1"}); err != nil {
		t.Fatalf("dispatch cli.run.wait: %v", err)
	}
	if eng.usedWaitRun {
		t.Error("cli.run.wait used WaitRun; the follow loop would block past a suspended continuation")
	}
	if !eng.usedWaitSettle {
		t.Fatal("cli.run.wait did not use WaitRunSettled")
	}
}
