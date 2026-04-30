package trigger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
)

func TestReplayRunner_FiresWithReplaySource(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)

	spec := writeTask(t, dir, "echo-task", `export default async function main(opts) {
	  return opts && opts.input ? opts.input : "no-input"
	}`, task.TriggerConfig{Manual: true})
	if err := e.reg.Register(spec); err != nil {
		t.Fatal(err)
	}

	// Fire an original run so we have a parent-run-id.
	parentRunID, err := e.engine.FireManual(context.Background(), "echo-task", nil)
	if err != nil {
		t.Fatal(err)
	}
	waitForTerminal(t, e.engine, parentRunID, 30*time.Second)

	// Use the adapter to fire a replay.
	runner := NewReplayRunner(e.engine)

	newRunID, err := runner.FireForReplay(
		context.Background(),
		"echo-task",
		parentRunID,
		map[string]any{"replayed": true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if newRunID == "" {
		t.Fatal("new run ID empty")
	}

	got := waitForTerminal(t, e.engine, newRunID, 30*time.Second)
	if got.TriggerSource != "replay" {
		t.Errorf("TriggerSource = %q, want replay", got.TriggerSource)
	}
	if got.ParentRunID != parentRunID {
		t.Errorf("ParentRunID = %q, want %q", got.ParentRunID, parentRunID)
	}
}

func TestReplayRunner_TaskNotRegistered(t *testing.T) {
	e := newTestEnv(t)
	runner := NewReplayRunner(e.engine)

	_, err := runner.FireForReplay(
		context.Background(),
		"nonexistent-task",
		"parent-run-id",
		nil,
	)
	if err == nil {
		t.Fatal("expected error for unregistered task")
	}
	// Verify it's the typed error.
	var notFound *TaskNotFoundError
	if !errors.As(err, &notFound) {
		t.Errorf("got %v (%T), want *TaskNotFoundError", err, err)
	}

	// Suppress unused: registry import may not be needed here.
	_ = registry.StatusRunning
}
