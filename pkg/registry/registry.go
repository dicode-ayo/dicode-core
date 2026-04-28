// Package registry holds the in-memory task registry and sqlite-backed run log.
package registry

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/task"
	"github.com/google/uuid"
)

// ErrRunNotFound is returned by GetRun when no run record exists for the given ID.
var ErrRunNotFound = errors.New("run not found")

// RunStatus values.
const (
	StatusRunning   = "running"
	StatusSuccess   = "success"
	StatusFailure   = "failure"
	StatusCancelled = "cancelled"
)

// Run is a single execution record.
type Run struct {
	ID            string
	TaskID        string
	Status        string
	StartedAt     time.Time
	FinishedAt    *time.Time
	ParentRunID   string
	TriggerSource string
	ReturnValue   string // JSON-encoded return value; empty if none

	// Structured output produced by output.html() / output.text().
	OutputContentType string
	OutputContent     string

	// FailureReason is a typed reason string set when Status == StatusFailure.
	// Format: "<category>: <detail>", e.g. "provider_unavailable: doppler"
	// or "required_secret_missing: PG_URL from doppler". Empty for non-failed
	// runs and for failures from the legacy code path that doesn't set a reason.
	FailureReason string
}

// LogEntry is one log line from a run.
type LogEntry struct {
	ID      int64
	RunID   string
	Ts      time.Time
	Level   string
	Message string
}

// Registry is an in-memory map of tasks backed by a sqlite run log.
type Registry struct {
	mu      sync.RWMutex
	tasks   map[string]*task.Spec
	db      db.DB
	logHook func(runID, level, msg string, ts int64)
	logMu   sync.Mutex
}

// New creates an empty Registry backed by the given DB.
func New(database db.DB) *Registry {
	return &Registry{
		tasks: make(map[string]*task.Spec),
		db:    database,
	}
}

// Register upserts a task spec into the registry.
func (r *Registry) Register(spec *task.Spec) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tasks[spec.ID] = spec
	return nil
}

// Unregister removes a task from the registry.
func (r *Registry) Unregister(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tasks, id)
}

// Get returns the spec for a task ID, or (nil, false) if not found.
func (r *Registry) Get(id string) (*task.Spec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.tasks[id]
	return s, ok
}

// All returns a snapshot of all registered task specs sorted by ID.
func (r *Registry) All() []*task.Spec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*task.Spec, 0, len(r.tasks))
	for _, s := range r.tasks {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// StartRun records a new run in sqlite and returns its ID.
func (r *Registry) StartRun(ctx context.Context, taskID, parentRunID string) (string, error) {
	return r.StartRunWithID(ctx, uuid.New().String(), taskID, parentRunID, "")
}

// StartRunWithID records a new run using a caller-supplied ID.
// Use this when the run ID must be known before execution begins (e.g. async fire).
func (r *Registry) StartRunWithID(ctx context.Context, id, taskID, parentRunID, triggerSource string) (string, error) {
	now := time.Now().UnixMilli()
	err := r.db.Exec(ctx,
		`INSERT INTO runs (id, task_id, status, started_at, parent_run_id, trigger_source) VALUES (?, ?, ?, ?, ?, ?)`,
		id, taskID, StatusRunning, now, parentRunID, triggerSource,
	)
	if err != nil {
		return "", fmt.Errorf("start run: %w", err)
	}
	return id, nil
}

// SetRunResult stores a JSON-encoded return value and optional structured output for a finished run.
func (r *Registry) SetRunResult(ctx context.Context, runID, returnValueJSON, outputContentType, outputContent string) error {
	return r.db.Exec(ctx,
		`UPDATE runs SET return_value = ?, output_content_type = ?, output_content = ? WHERE id = ?`,
		returnValueJSON, outputContentType, outputContent, runID,
	)
}

// FinishRun updates the run status and finished_at timestamp.
func (r *Registry) FinishRun(ctx context.Context, runID, status string) error {
	now := time.Now().UnixMilli()
	return r.db.Exec(ctx,
		`UPDATE runs SET status = ?, finished_at = ? WHERE id = ?`,
		status, now, runID,
	)
}

// FinishRunWithReason updates run status, finished_at, AND fail_reason.
// Used by the trigger engine when env resolution fails with a typed
// envresolve error before the consumer process is even spawned.
func (r *Registry) FinishRunWithReason(ctx context.Context, runID, status, reason string) error {
	now := time.Now().UnixMilli()
	return r.db.Exec(ctx,
		`UPDATE runs SET status = ?, finished_at = ?, fail_reason = ? WHERE id = ?`,
		status, now, reason, runID,
	)
}

// SetLogHook registers a function called after each log entry is written.
func (r *Registry) SetLogHook(fn func(runID, level, msg string, ts int64)) {
	r.logMu.Lock()
	r.logHook = fn
	r.logMu.Unlock()
}

// AppendLog adds a log entry for a run.
func (r *Registry) AppendLog(ctx context.Context, runID, level, msg string) error {
	now := time.Now().UnixMilli()
	if err := r.db.Exec(ctx,
		`INSERT INTO run_logs (run_id, ts, level, message) VALUES (?, ?, ?, ?)`,
		runID, now, level, msg,
	); err != nil {
		return err
	}
	r.logMu.Lock()
	hook := r.logHook
	r.logMu.Unlock()
	if hook != nil {
		hook(runID, level, msg, now)
	}
	return nil
}

// PendingLogEntry holds a log line waiting to be flushed to the DB.
// It captures the timestamp at enqueue time so ordering is preserved even
// if the flush goroutine is delayed.
type PendingLogEntry struct {
	RunID   string
	Level   string
	Message string
	TsMs    int64 // Unix milliseconds, captured at enqueue time
}

// BulkAppendLogs inserts a batch of log entries in a single transaction.
// Entries may belong to different run IDs; insertion order within the batch is
// preserved by the AUTOINCREMENT rowid assigned by SQLite.
func (r *Registry) BulkAppendLogs(ctx context.Context, entries []PendingLogEntry) error {
	if len(entries) == 0 {
		return nil
	}

	// Always use the bulk path (even for a single entry) so that the
	// pre-captured TsMs is written instead of time.Now() from AppendLog.
	// Wrap all inserts in a single transaction so they land atomically
	// and only one fsync is needed per batch.
	err := r.db.Tx(ctx, func(tx db.DB) error {
		for _, e := range entries {
			if err := tx.Exec(ctx,
				`INSERT INTO run_logs (run_id, ts, level, message) VALUES (?, ?, ?, ?)`,
				e.RunID, e.TsMs, e.Level, e.Message,
			); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Fire the log hook (if any) for each entry after the transaction commits.
	r.logMu.Lock()
	hook := r.logHook
	r.logMu.Unlock()
	if hook != nil {
		for _, e := range entries {
			hook(e.RunID, e.Level, e.Message, e.TsMs)
		}
	}
	return nil
}

// GetRun fetches a run record by ID.
func (r *Registry) GetRun(ctx context.Context, runID string) (*Run, error) {
	var run *Run
	err := r.db.Query(ctx,
		`SELECT id, task_id, status, started_at, finished_at, parent_run_id, trigger_source,
		        COALESCE(return_value, ''), COALESCE(output_content_type, ''), COALESCE(output_content, ''),
		        COALESCE(fail_reason, '')
		 FROM runs WHERE id = ?`,
		[]any{runID},
		func(rows db.Scanner) error {
			if rows.Next() {
				run = &Run{}
				var startedMs int64
				var finishedMs *int64
				var parentID *string
				if err := rows.Scan(&run.ID, &run.TaskID, &run.Status, &startedMs, &finishedMs, &parentID, &run.TriggerSource, &run.ReturnValue, &run.OutputContentType, &run.OutputContent, &run.FailureReason); err != nil {
					return err
				}
				run.StartedAt = time.UnixMilli(startedMs)
				if finishedMs != nil {
					t := time.UnixMilli(*finishedMs)
					run.FinishedAt = &t
				}
				if parentID != nil {
					run.ParentRunID = *parentID
				}
			}
			return nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("get run %s: %w", runID, err)
	}
	if run == nil {
		return nil, fmt.Errorf("run %s: %w", runID, ErrRunNotFound)
	}
	return run, nil
}

// ListRuns returns the most recent runs for a task (newest first).
func (r *Registry) ListRuns(ctx context.Context, taskID string, limit int) ([]*Run, error) {
	var runs []*Run
	err := r.db.Query(ctx,
		`SELECT id, task_id, status, started_at, finished_at, parent_run_id, trigger_source,
		        COALESCE(return_value, ''), COALESCE(output_content_type, ''), COALESCE(output_content, ''),
		        COALESCE(fail_reason, '')
		 FROM runs WHERE task_id = ? ORDER BY started_at DESC LIMIT ?`,
		[]any{taskID, limit},
		func(rows db.Scanner) error {
			for rows.Next() {
				run := &Run{}
				var startedMs int64
				var finishedMs *int64
				var parentID *string
				if err := rows.Scan(&run.ID, &run.TaskID, &run.Status, &startedMs, &finishedMs, &parentID, &run.TriggerSource, &run.ReturnValue, &run.OutputContentType, &run.OutputContent, &run.FailureReason); err != nil {
					return err
				}
				run.StartedAt = time.UnixMilli(startedMs)
				if finishedMs != nil {
					t := time.UnixMilli(*finishedMs)
					run.FinishedAt = &t
				}
				if parentID != nil {
					run.ParentRunID = *parentID
				}
				runs = append(runs, run)
			}
			return nil
		},
	)
	return runs, err
}

// CleanupStaleRuns marks any "running" runs as "cancelled".
// Called at startup to handle runs from a previous session that never finished.
// Returns the distinct task IDs that had stale runs so callers can restart them.
func (r *Registry) CleanupStaleRuns(ctx context.Context) ([]string, error) {
	var taskIDs []string
	err := r.db.Query(ctx,
		`SELECT DISTINCT task_id FROM runs WHERE status = ?`,
		[]any{StatusRunning},
		func(rows db.Scanner) error {
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					return err
				}
				taskIDs = append(taskIDs, id)
			}
			return nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("query stale runs: %w", err)
	}
	if len(taskIDs) == 0 {
		return nil, nil
	}
	now := time.Now().UnixMilli()
	if err := r.db.Exec(ctx,
		`UPDATE runs SET status = ?, finished_at = ? WHERE status = ?`,
		StatusCancelled, now, StatusRunning,
	); err != nil {
		return nil, fmt.Errorf("cancel stale runs: %w", err)
	}
	return taskIDs, nil
}

// GetRunLogs returns all log entries for a run ordered by ID ascending.
func (r *Registry) GetRunLogs(ctx context.Context, runID string) ([]*LogEntry, error) {
	return r.getRunLogsQuery(ctx, runID, 0)
}

// GetRunLogsSince returns log entries for a run with ID greater than sinceID.
// Used for incremental polling so callers only receive new lines.
func (r *Registry) GetRunLogsSince(ctx context.Context, runID string, sinceID int64) ([]*LogEntry, error) {
	return r.getRunLogsQuery(ctx, runID, sinceID)
}

func (r *Registry) getRunLogsQuery(ctx context.Context, runID string, sinceID int64) ([]*LogEntry, error) {
	var logs []*LogEntry
	err := r.db.Query(ctx,
		`SELECT id, run_id, ts, level, message FROM run_logs WHERE run_id = ? AND id > ? ORDER BY id ASC`,
		[]any{runID, sinceID},
		func(rows db.Scanner) error {
			for rows.Next() {
				e := &LogEntry{}
				var tsMs int64
				if err := rows.Scan(&e.ID, &e.RunID, &tsMs, &e.Level, &e.Message); err != nil {
					return err
				}
				e.Ts = time.UnixMilli(tsMs)
				logs = append(logs, e)
			}
			return nil
		},
	)
	return logs, err
}
