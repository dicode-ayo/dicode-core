package registry

import (
	"context"
	"errors"
	"fmt"
)

// ErrReplayNotPermitted is returned by Replay when the caller's task identity
// does not match the run's task and there is no parent-run lineage that would
// allow it. See ownership policy in Replay's doc comment.
var ErrReplayNotPermitted = errors.New("replay: caller task may not replay this run")

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

// fetcher abstracts InputStore.Fetch for testability. *InputStore satisfies
// this interface without modification.
type fetcher interface {
	Fetch(ctx context.Context, runID, key string, storedAt int64) (PersistedInput, error)
}

// Replayer fetches a persisted input and re-fires its task (or an override
// task) with that input. The new run carries triggerSource = "replay" so
// the trigger engine skips chain-firing on its failure (per spec § 4.3).
type Replayer struct {
	registry *Registry
	store    fetcher
	runner   ReplayRunner
}

// NewReplayer returns a Replayer wired against the given registry, input
// store, and runner.
func NewReplayer(reg *Registry, store fetcher, runner ReplayRunner) *Replayer {
	return &Replayer{registry: reg, store: store, runner: runner}
}

// Replay fetches runID's persisted input and fires it against the original
// task (or override taskName when non-empty). Returns the new run ID.
//
// Ownership policy (#246): when callerTaskID or callerParentRunID is non-empty,
// the caller is a task-scoped IPC client and the call is permitted only when:
//   - run.TaskID == callerTaskID (a task replaying its own historical run), OR
//   - callerParentRunID == runID  (a chain-fired task replaying its parent run).
//
// When both fields are empty the ownership check is bypassed — backwards
// compatible for REST handlers and tests that have no task scope.
//
// Errors:
//   - run not found → wrapped GetRun error
//   - run has no persisted input → ErrInputUnavailable
//   - ownership check fails → ErrReplayNotPermitted
//   - fetch/decrypt failure → wrapped fetch error
//   - runner failure → wrapped fire error
func (r *Replayer) Replay(ctx context.Context, runID, taskName, callerTaskID, callerParentRunID string) (string, error) {
	run, err := r.registry.GetRun(ctx, runID)
	if err != nil {
		return "", fmt.Errorf("get run: %w", err)
	}
	if run.InputStorageKey == "" {
		return "", ErrInputUnavailable
	}

	// Ownership check: only enforce when the caller has a task identity.
	if callerTaskID != "" || callerParentRunID != "" {
		if run.TaskID != callerTaskID && callerParentRunID != runID {
			return "", ErrReplayNotPermitted
		}
		// Even when ownership passes, forbid redirecting the replay at a
		// different task — a task with RunsReplay could otherwise inject the
		// current run's input into a broader-privileged sibling.
		if taskName != "" && taskName != run.TaskID {
			return "", ErrReplayNotPermitted
		}
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
