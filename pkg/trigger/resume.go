package trigger

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"go.uber.org/zap"
)

// defaultResumeTTL is how long a suspended run stays resumable when the task
// did not specify its own deadline. After it, the sweep cancels the run with
// ReasonResumeTimeout.
const defaultResumeTTL = 24 * time.Hour

// Resume errors surfaced to callers (webui/CLI in later PRs).
var (
	// ErrResumeTokenNotFound is returned when no suspended run carries the token.
	ErrResumeTokenNotFound = errors.New("resume token not found")
	// ErrResumeNotSuspended is returned when the token's run is no longer
	// suspended — already resumed (single-use token replayed), cancelled, or
	// finished.
	ErrResumeNotSuspended = errors.New("run is not suspended")
	// ErrResumeExpired is returned when the run's resume_deadline has passed.
	// The run is swept to cancelled/resume_timeout as a side effect.
	ErrResumeExpired = errors.New("resume deadline expired")
)

// suspendRun persists a run that called dicode.suspend() as suspended: it mints
// an unguessable single-use resume token and records the state/form blobs plus
// the deadline. The deadline comes from the task (result.ResumeDeadline) or
// defaults to defaultResumeTTL from now.
func (e *Engine) suspendRun(runID string, result *pkgruntime.RunResult) error {
	token, err := newResumeToken()
	if err != nil {
		return fmt.Errorf("mint resume token: %w", err)
	}
	nowMs := time.Now().UnixMilli()
	deadlineMs := result.ResumeDeadline
	if deadlineMs <= 0 {
		deadlineMs = time.Now().Add(defaultResumeTTL).UnixMilli()
	}
	return e.registry.SuspendRun(context.Background(), runID,
		result.ResumeState, result.ResumeForm, token, nowMs, deadlineMs)
}

// newResumeToken returns a 32-byte crypto/rand token, hex-encoded. Long and
// unpredictable enough to serve as the resume authorization handle.
func newResumeToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// ResumeRun consumes a resume token and spawns the continuation run for a
// suspended task. It looks the run up by token, rejects it if not found, not
// suspended, or past its deadline (an expired run is swept to
// cancelled/resume_timeout), marks the original run resumed so the token can't
// be replayed, then fires a fresh run of the same task seeded with the stored
// resume state and the caller's input. The continuation runs the normal
// execution path, so a task that suspends again mints its own token — chaining
// multi-step wizards. Returns the continuation run's ID.
func (e *Engine) ResumeRun(ctx context.Context, token string, input []byte) (string, error) {
	run, err := e.registry.GetRunByResumeToken(ctx, token)
	if err != nil {
		if errors.Is(err, registry.ErrRunNotFound) {
			return "", ErrResumeTokenNotFound
		}
		return "", err
	}
	if run.Status != registry.StatusSuspended {
		return "", ErrResumeNotSuspended
	}
	if run.ResumeDeadline > 0 && time.Now().UnixMilli() > run.ResumeDeadline {
		// Expired: sweep it now so its terminal state reflects the timeout even
		// if the periodic sweep hasn't run yet, then reject.
		if _, serr := e.registry.SweepExpiredSuspensions(ctx, time.Now().UnixMilli()); serr != nil {
			e.log.Warn("resume: sweep expired suspension failed",
				zap.String("run", run.ID), zap.Error(serr))
		}
		return "", ErrResumeExpired
	}

	// Consume the token atomically. This is the single-use guard: a second
	// ResumeRun for the same token finds the run already resumed and fails.
	if err := e.registry.MarkRunResumed(ctx, run.ID); err != nil {
		if errors.Is(err, registry.ErrRunNotSuspended) {
			return "", ErrResumeNotSuspended
		}
		return "", fmt.Errorf("mark run resumed: %w", err)
	}

	spec, ok := e.registry.Get(run.TaskID)
	if !ok {
		return "", fmt.Errorf("resume: task %q is no longer registered", run.TaskID)
	}

	newRunID, err := e.fireAsync(context.Background(), spec, pkgruntime.RunOptions{
		ParentRunID: run.ID,
		ResumeState: run.ResumeState,
		ResumeInput: input,
	}, registry.TriggerResume)
	if err != nil {
		return "", fmt.Errorf("resume: spawn continuation run: %w", err)
	}
	e.log.Info("run resumed",
		zap.String("task", run.TaskID),
		zap.String("suspended_run", run.ID),
		zap.String("continuation_run", newRunID),
	)
	return newRunID, nil
}
