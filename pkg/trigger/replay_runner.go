package trigger

import (
	"context"
	"fmt"

	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
)

// ReplayRunnerAdapter wraps an Engine to satisfy registry.ReplayRunner.
// FireForReplay calls Engine.fireAsync with source = "replay" and the
// supplied parent_run_id; the engine's existing chain-suppression guard
// (introduced in #236) skips on_failure_chain for replay-sourced runs.
type ReplayRunnerAdapter struct {
	engine *Engine
}

// NewReplayRunner constructs a ReplayRunnerAdapter for the given engine.
// Returns the registry.ReplayRunner interface so callers don't depend on
// the concrete adapter type.
func NewReplayRunner(engine *Engine) registry.ReplayRunner {
	return &ReplayRunnerAdapter{engine: engine}
}

// FireForReplay implements registry.ReplayRunner.
func (a *ReplayRunnerAdapter) FireForReplay(ctx context.Context, taskID, parentRunID string, input any) (string, error) {
	spec, ok := a.engine.registry.Get(taskID)
	if !ok {
		return "", &TaskNotFoundError{TaskID: taskID}
	}
	return a.engine.fireAsync(ctx, spec, pkgruntime.RunOptions{
		ParentRunID: parentRunID,
		Input:       input,
	}, "replay")
}

// TaskNotFoundError signals that the requested replay target task is not
// registered. Surfaced through the IPC and REST layers as 404.
type TaskNotFoundError struct {
	TaskID string
}

func (e *TaskNotFoundError) Error() string {
	return fmt.Sprintf("task not registered: %s", e.TaskID)
}
