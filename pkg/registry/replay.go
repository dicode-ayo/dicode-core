package registry

import (
	"context"
	"fmt"
)

// ReplayRunner abstracts the trigger engine's ability to fire a task with a
// given input as a "replay" source. Decoupled from pkg/trigger via this
// interface to keep pkg/registry import-cycle-free. The trigger engine's
// adapter (pkg/trigger.ReplayRunnerAdapter) implements this interface.
type ReplayRunner interface {
	// FireForReplay fires the given task with input attached, sets
	// triggerSource = "replay" on the new run, sets parent_run_id =
	// parentRunID. Returns the new run ID synchronously; the run executes
	// asynchronously.
	FireForReplay(ctx context.Context, taskID, parentRunID string, input any) (string, error)
}

// Replayer fetches a persisted input and re-fires its task (or an override
// task) with that input. The new run carries triggerSource = "replay" so
// the trigger engine skips chain-firing on its failure (per spec § 4.3).
type Replayer struct {
	registry *Registry
	store    *InputStore
	runner   ReplayRunner
}

// NewReplayer returns a Replayer wired against the given registry, input
// store, and runner.
func NewReplayer(reg *Registry, store *InputStore, runner ReplayRunner) *Replayer {
	return &Replayer{registry: reg, store: store, runner: runner}
}

// Replay fetches runID's persisted input and fires it against the original
// task (or override taskName when non-empty). Returns the new run ID.
//
// Errors:
//   - run not found → wrapped GetRun error
//   - run has no persisted input → ErrInputUnavailable
//   - fetch/decrypt failure → wrapped fetch error
//   - runner failure → wrapped fire error
func (r *Replayer) Replay(ctx context.Context, runID, taskName string) (string, error) {
	run, err := r.registry.GetRun(ctx, runID)
	if err != nil {
		return "", fmt.Errorf("get run: %w", err)
	}
	if run.InputStorageKey == "" {
		return "", ErrInputUnavailable
	}

	in, err := r.store.Fetch(ctx, runID, run.InputStorageKey, run.InputStoredAt)
	if err != nil {
		return "", fmt.Errorf("fetch input: %w", err)
	}

	target := run.TaskID
	if taskName != "" {
		target = taskName
	}

	newRunID, err := r.runner.FireForReplay(ctx, target, runID, in)
	if err != nil {
		return "", fmt.Errorf("fire replay: %w", err)
	}
	return newRunID, nil
}
