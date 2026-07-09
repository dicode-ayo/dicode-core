package trigger

// #533 follow-up: apiReplayRun holds a DrainSlot across Replayer.Replay, whose
// last step (FireForReplay → fireAsync) reserves its own trackRun slot. If
// shutdown latches in the window between the outer DrainSlot and that inner
// reserve, the inner fire is refused. This asserts the refusal is clean: a
// sentinel error, no orphan run row, and the outer slot still releases exactly
// once with runWG balanced (no leak, no over-decrement).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
)

func TestDrainSlotOuterHeldInnerFireRefusedIsClean(t *testing.T) {
	exec := &drainExec{started: make(chan struct{})}
	eng, reg := newDrainEnv(t, exec)

	spec := &task.Spec{
		ID:      "replay-target",
		Name:    "replay-target",
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{Manual: true},
		Enabled: true,
	}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("reg.Register: %v", err)
	}

	// Outer top-level slot — models apiReplayRun's DrainSlot held across Replay.
	release, ok := eng.DrainSlot()
	if !ok {
		t.Fatal("DrainSlot refused before shutdown latched")
	}

	// Shutdown latches inside the window between the outer reserve and the inner
	// fire below.
	eng.beginShutdown()

	// Inner fire — models FireForReplay → fireAsync — must be refused cleanly
	// (fireAsync reserves its slot before creating any run record).
	runID, err := eng.fireAsync(context.Background(), spec, pkgruntime.RunOptions{}, registry.TriggerReplay)
	if !errors.Is(err, errEngineShuttingDown) {
		t.Fatalf("fireAsync err = %v, want errEngineShuttingDown", err)
	}
	if runID != "" {
		t.Fatalf("fireAsync returned runID %q; a refused fire must start no run", runID)
	}
	runs, err := reg.ListRuns(context.Background(), spec.ID, 10)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("refused inner fire left %d run row(s); it must create none", len(runs))
	}

	// The outer slot still owns exactly one release: after it fires, runWG must
	// return to balanced — the refused inner fire added nothing to leak.
	release()
	drained := make(chan struct{})
	go func() { eng.runWG.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("runWG never drained after outer release — a slot leaked")
	}

	// Idempotent release: a second (accidental) defer must not over-decrement
	// runWG into a negative-counter panic.
	release()
}
