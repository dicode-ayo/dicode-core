// Package registry holds the in-memory task registry and sqlite-backed run log.
package registry

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/task"
	"github.com/google/uuid"
)

// RunStatus values.
const (
	StatusRunning = "running"
	StatusSuccess = "success"
	StatusFailure = "failure"
)

// Run is a single execution record.
type Run struct {
	ID          string
	TaskID      string
	Status      string
	StartedAt   time.Time
	FinishedAt  *time.Time
	ParentRunID string
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
	mu    sync.RWMutex
	tasks map[string]*task.Spec
	db    db.DB
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

// All returns a snapshot of all registered task specs.
func (r *Registry) All() []*task.Spec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*task.Spec, 0, len(r.tasks))
	for _, s := range r.tasks {
		out = append(out, s)
	}
	return out
}

// StartRun records a new run in sqlite and returns its ID.
func (r *Registry) StartRun(ctx context.Context, taskID, parentRunID string) (string, error) {
	id := uuid.New().String()
	now := time.Now().UnixMilli()
	err := r.db.Exec(ctx,
		`INSERT INTO runs (id, task_id, status, started_at, parent_run_id) VALUES (?, ?, ?, ?, ?)`,
		id, taskID, StatusRunning, now, parentRunID,
	)
	if err != nil {
		return "", fmt.Errorf("start run: %w", err)
	}
	return id, nil
}

// FinishRun updates the run status and finished_at timestamp.
func (r *Registry) FinishRun(ctx context.Context, runID, status string) error {
	now := time.Now().UnixMilli()
	return r.db.Exec(ctx,
		`UPDATE runs SET status = ?, finished_at = ? WHERE id = ?`,
		status, now, runID,
	)
}

// AppendLog adds a log entry for a run.
func (r *Registry) AppendLog(ctx context.Context, runID, level, msg string) error {
	now := time.Now().UnixMilli()
	return r.db.Exec(ctx,
		`INSERT INTO run_logs (run_id, ts, level, message) VALUES (?, ?, ?, ?)`,
		runID, now, level, msg,
	)
}

// GetRun fetches a run record by ID.
func (r *Registry) GetRun(ctx context.Context, runID string) (*Run, error) {
	var run *Run
	err := r.db.Query(ctx,
		`SELECT id, task_id, status, started_at, finished_at, parent_run_id FROM runs WHERE id = ?`,
		[]any{runID},
		func(rows db.Scanner) error {
			if rows.Next() {
				run = &Run{}
				var startedMs int64
				var finishedMs *int64
				var parentID *string
				if err := rows.Scan(&run.ID, &run.TaskID, &run.Status, &startedMs, &finishedMs, &parentID); err != nil {
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
		return nil, fmt.Errorf("run %s not found", runID)
	}
	return run, nil
}

// ListRuns returns the most recent runs for a task (newest first).
func (r *Registry) ListRuns(ctx context.Context, taskID string, limit int) ([]*Run, error) {
	var runs []*Run
	err := r.db.Query(ctx,
		`SELECT id, task_id, status, started_at, finished_at, parent_run_id
		 FROM runs WHERE task_id = ? ORDER BY started_at DESC LIMIT ?`,
		[]any{taskID, limit},
		func(rows db.Scanner) error {
			for rows.Next() {
				run := &Run{}
				var startedMs int64
				var finishedMs *int64
				var parentID *string
				if err := rows.Scan(&run.ID, &run.TaskID, &run.Status, &startedMs, &finishedMs, &parentID); err != nil {
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

// GetRunLogs returns log entries for a run.
func (r *Registry) GetRunLogs(ctx context.Context, runID string) ([]*LogEntry, error) {
	var logs []*LogEntry
	err := r.db.Query(ctx,
		`SELECT id, run_id, ts, level, message FROM run_logs WHERE run_id = ? ORDER BY id ASC`,
		[]any{runID},
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
