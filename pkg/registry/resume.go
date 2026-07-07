package registry

import (
	"context"
	"fmt"
	"time"

	"github.com/dicode/dicode/pkg/db"
)

// GetRunByResumeToken fetches the run holding the given resume token. Returns
// ErrRunNotFound when no run carries the token. A token is unique per suspend
// (minted from crypto/rand by the engine), so at most one row matches.
func (r *Registry) GetRunByResumeToken(ctx context.Context, token string) (*Run, error) {
	if token == "" {
		return nil, ErrRunNotFound
	}
	var run *Run
	err := r.db.Query(ctx,
		`SELECT `+runColumns+runInputColumns+runResumeColumns+` FROM runs WHERE resume_token = ?`,
		[]any{token},
		func(rows db.Scanner) error {
			if rows.Next() {
				var scanErr error
				run, scanErr = scanRun(rows, true, true)
				return scanErr
			}
			return nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("get run by resume token: %w", err)
	}
	if run == nil {
		return nil, ErrRunNotFound
	}
	return run, nil
}

// ListSuspendedRuns returns every run currently in the suspended state, newest
// first (by suspend time). Unlike queryRuns, it selects the resume columns so
// callers can render each run's pending form — used by `dicode resume` to list
// what's resumable.
func (r *Registry) ListSuspendedRuns(ctx context.Context, limit int) ([]*Run, error) {
	var runs []*Run
	err := r.db.Query(ctx,
		`SELECT `+runColumns+runInputColumns+runResumeColumns+`
		 FROM runs WHERE status = ? ORDER BY suspended_at DESC LIMIT ?`,
		[]any{StatusSuspended, limit},
		func(rows db.Scanner) error {
			for rows.Next() {
				run, err := scanRun(rows, true, true)
				if err != nil {
					return err
				}
				runs = append(runs, run)
			}
			return nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("list suspended runs: %w", err)
	}
	return runs, nil
}

// MarkRunResumed transitions a suspended run to the terminal `resumed` state,
// consuming its resume token so it can't be replayed. The transition is a
// single conditional UPDATE gated on the current status; RowsAffected decides
// the outcome, so exactly one of two concurrent resumes of the same token wins
// (the loser changes 0 rows and gets ErrRunNotSuspended). This is atomic and
// correct regardless of the connection-pool size — no SELECT-then-UPDATE window.
// Returns ErrRunNotSuspended when the run isn't currently suspended (already
// resumed, cancelled, or never suspended) and ErrRunNotFound when no such run
// exists.
func (r *Registry) MarkRunResumed(ctx context.Context, runID string) error {
	now := time.Now().UnixMilli()
	affected, err := r.db.ExecResult(ctx,
		`UPDATE runs SET status = ?, finished_at = ? WHERE id = ? AND status = ?`,
		StatusResumed, now, runID, StatusSuspended,
	)
	if err != nil {
		return fmt.Errorf("mark run resumed: %w", err)
	}
	if affected > 0 {
		return nil
	}
	// The guarded UPDATE changed nothing: the run isn't suspended. Distinguish
	// "no such run" from "present but not suspended" for a precise error — this
	// is diagnostic only; the single-use guard was already enforced atomically
	// by the conditional UPDATE above.
	var exists bool
	if err := r.db.Query(ctx,
		`SELECT 1 FROM runs WHERE id = ?`,
		[]any{runID},
		func(rows db.Scanner) error {
			exists = rows.Next()
			return nil
		},
	); err != nil {
		return fmt.Errorf("check run exists: %w", err)
	}
	if !exists {
		return ErrRunNotFound
	}
	return ErrRunNotSuspended
}

// CancelSuspendedRun transitions a suspended run to cancelled with the given
// fail_reason and clears its resume token, but ONLY while the run is still
// suspended. The conditional UPDATE mirrors MarkRunResumed/SweepExpiredSuspensions:
// an operator resume racing this finalize (which flips suspended→resumed) changes
// 0 rows here and is not clobbered back to cancelled. Reports whether the row
// changed. Used to finalize a pipeline stage that suspended, since a pipeline
// stage cannot pause its parent pipeline.
func (r *Registry) CancelSuspendedRun(ctx context.Context, runID, reason string) (bool, error) {
	now := time.Now().UnixMilli()
	affected, err := r.db.ExecResult(ctx,
		`UPDATE runs SET status = ?, finished_at = ?, fail_reason = ?, resume_token = NULL WHERE id = ? AND status = ?`,
		StatusCancelled, now, reason, runID, StatusSuspended,
	)
	if err != nil {
		return false, fmt.Errorf("cancel suspended run: %w", err)
	}
	return affected > 0, nil
}

// SweepExpiredSuspensions cancels every suspended run whose resume_deadline has
// passed (relative to nowMs), recording ReasonResumeTimeout as the fail_reason
// and stamping finished_at. Rows with no deadline (resume_deadline NULL/0) are
// left untouched. Returns the IDs of the runs it actually cancelled.
//
// Each candidate is transitioned with its own status-guarded UPDATE and only
// the rows that UPDATE changed are returned. A run that resumes between the
// candidate SELECT and its UPDATE changes 0 rows and is excluded, so a caller
// that fires finish side-effects per returned ID cannot double-finish a run
// that already left `suspended`.
func (r *Registry) SweepExpiredSuspensions(ctx context.Context, nowMs int64) ([]string, error) {
	var cancelled []string
	err := r.db.Tx(ctx, func(tx db.DB) error {
		cancelled = nil // reset on retry
		var candidates []string
		if err := tx.Query(ctx,
			`SELECT id FROM runs WHERE status = ? AND resume_deadline IS NOT NULL AND resume_deadline > 0 AND resume_deadline < ?`,
			[]any{StatusSuspended, nowMs},
			func(rows db.Scanner) error {
				for rows.Next() {
					var id string
					if err := rows.Scan(&id); err != nil {
						return err
					}
					candidates = append(candidates, id)
				}
				return nil
			},
		); err != nil {
			return fmt.Errorf("query expired suspensions: %w", err)
		}
		for _, id := range candidates {
			affected, err := tx.ExecResult(ctx,
				`UPDATE runs SET status = ?, finished_at = ?, fail_reason = ?
				 WHERE id = ? AND status = ?`,
				StatusCancelled, nowMs, ReasonResumeTimeout, id, StatusSuspended,
			)
			if err != nil {
				return fmt.Errorf("cancel expired suspension %s: %w", id, err)
			}
			if affected > 0 {
				cancelled = append(cancelled, id)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return cancelled, nil
}
