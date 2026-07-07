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

// MarkRunResumed transitions a suspended run to the terminal `resumed` state,
// consuming its resume token so it can't be replayed. The suspended→resumed
// UPDATE and the current-status check run in one transaction, so two concurrent
// resumes of the same token race to a single winner: the loser sees the row
// already resumed and gets ErrRunNotSuspended. Returns ErrRunNotSuspended when
// the run isn't currently suspended (already resumed, cancelled, or never
// suspended).
func (r *Registry) MarkRunResumed(ctx context.Context, runID string) error {
	now := time.Now().UnixMilli()
	return r.db.Tx(ctx, func(tx db.DB) error {
		var status string
		var found bool
		if err := tx.Query(ctx,
			`SELECT status FROM runs WHERE id = ?`,
			[]any{runID},
			func(rows db.Scanner) error {
				if rows.Next() {
					found = true
					return rows.Scan(&status)
				}
				return nil
			},
		); err != nil {
			return fmt.Errorf("read run status: %w", err)
		}
		if !found {
			return ErrRunNotFound
		}
		if status != StatusSuspended {
			return ErrRunNotSuspended
		}
		return tx.Exec(ctx,
			`UPDATE runs SET status = ?, finished_at = ? WHERE id = ?`,
			StatusResumed, now, runID,
		)
	})
}

// SweepExpiredSuspensions cancels every suspended run whose resume_deadline has
// passed (relative to nowMs), recording ReasonResumeTimeout as the fail_reason
// and stamping finished_at. Rows with no deadline (resume_deadline NULL/0) are
// left untouched. Returns the IDs of the runs it cancelled.
//
// The SELECT and UPDATE run in one transaction so a run that resumes between the
// two statements (flipping to `resumed`) is not erroneously cancelled.
func (r *Registry) SweepExpiredSuspensions(ctx context.Context, nowMs int64) ([]string, error) {
	var expired []string
	err := r.db.Tx(ctx, func(tx db.DB) error {
		expired = nil // reset on retry
		if err := tx.Query(ctx,
			`SELECT id FROM runs WHERE status = ? AND resume_deadline IS NOT NULL AND resume_deadline > 0 AND resume_deadline < ?`,
			[]any{StatusSuspended, nowMs},
			func(rows db.Scanner) error {
				for rows.Next() {
					var id string
					if err := rows.Scan(&id); err != nil {
						return err
					}
					expired = append(expired, id)
				}
				return nil
			},
		); err != nil {
			return fmt.Errorf("query expired suspensions: %w", err)
		}
		if len(expired) == 0 {
			return nil
		}
		return tx.Exec(ctx,
			`UPDATE runs SET status = ?, finished_at = ?, fail_reason = ?
			 WHERE status = ? AND resume_deadline IS NOT NULL AND resume_deadline > 0 AND resume_deadline < ?`,
			StatusCancelled, nowMs, ReasonResumeTimeout, StatusSuspended, nowMs,
		)
	})
	if err != nil {
		return nil, err
	}
	return expired, nil
}
