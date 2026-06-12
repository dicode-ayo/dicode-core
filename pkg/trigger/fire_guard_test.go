package trigger

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/task"
)

// TestFireGuardVetoesManualFire verifies that a registered fire guard blocks
// the registry-resolved fire paths (FireManual goes through fireKinded →
// startRun) before any run record is created.
func TestFireGuardVetoesManualFire(t *testing.T) {
	env := newTestEnv(t)
	spec := writeTask(t, t.TempDir(), "guarded", "dicode.log('hi');", task.TriggerConfig{Manual: true})
	if err := env.reg.Register(spec); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := env.engine.Register(spec); err != nil {
		t.Fatalf("engine register: %v", err)
	}

	env.engine.SetFireGuard(func(taskID string) error {
		if taskID == "guarded" {
			return fmt.Errorf("task pending approval: %s", taskID)
		}
		return nil
	})

	if _, err := env.engine.FireManual(context.Background(), "guarded", nil); err == nil ||
		!strings.Contains(err.Error(), "pending approval") {
		t.Fatalf("FireManual = %v, want pending-approval veto", err)
	}

	// Removing the guard restores normal behaviour.
	env.engine.SetFireGuard(nil)
	runID, err := env.engine.FireManual(context.Background(), "guarded", nil)
	if err != nil {
		t.Fatalf("FireManual after guard removed: %v", err)
	}
	if runID == "" {
		t.Fatal("expected a run ID")
	}
}
