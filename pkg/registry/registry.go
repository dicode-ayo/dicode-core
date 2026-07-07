// Package registry holds the in-memory task registry and sqlite-backed run log.
package registry

import (
	"context"
	"encoding/json"
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

// ErrRunNotSuspended is returned by MarkRunResumed when the target run is not
// in the suspended state — the single-use guard that makes a resume token
// non-replayable (a second resume of the same run finds it already resumed).
var ErrRunNotSuspended = errors.New("run not suspended")

// RunStatus values. success/failure/cancelled/resumed are terminal; running and
// suspended are not. A suspended run has paused awaiting user input — it is
// neither in-flight (not "running", so CleanupStaleRuns leaves it alone) nor
// finished (finished_at stays NULL until it resumes or its deadline expires).
// A suspended run transitions to resumed exactly once, when its token is
// consumed to spawn the continuation run — after which the token can't be
// replayed.
const (
	StatusRunning   = "running"
	StatusSuccess   = "success"
	StatusFailure   = "failure"
	StatusCancelled = "cancelled"
	StatusSuspended = "suspended"
	StatusResumed   = "resumed"
)

// ReasonResumeTimeout is the fail_reason recorded when the deadline sweep
// cancels a suspended run whose resume_deadline has passed.
const ReasonResumeTimeout = "resume_timeout"

// Run kind values for the runs.kind column. These are the lowercase run-row
// discriminators — distinct from pkg/task's task-kind strings ("Task" /
// "PipelineTask"). RunKindPipeline is written by the PipelineTask dispatcher
// (a later PR); everything else is RunKindTask.
const (
	RunKindTask     = "task"
	RunKindPipeline = "pipeline"
)

// Run is a single execution record.
type Run struct {
	ID          string
	TaskID      string
	Kind        string // run kind: "task" (default) or "pipeline" (see RunKindTask/RunKindPipeline)
	Status      string
	StartedAt   time.Time
	FinishedAt  *time.Time
	ParentRunID string
	// Group is a free-text label set by the task itself via dicode.set_group().
	// Used by the WebUI to collapse same-group siblings in the run list (#114).
	// Column name on disk is `run_group` because GROUP is a SQL keyword.
	Group         string
	TriggerSource TriggerSource
	ReturnValue   string // JSON-encoded return value; empty if none

	// Structured output produced by output.html() / output.text().
	OutputContentType string
	OutputContent     string

	// FailureReason is a typed reason string set when Status == StatusFailure.
	// Format: "<category>: <detail>", e.g. "provider_unavailable: doppler"
	// or "required_secret_missing: PG_URL from doppler". Empty for non-failed
	// runs and for failures from the legacy code path that doesn't set a reason.
	FailureReason string

	// Input persistence fields — set by the trigger engine via SetRunInput
	// immediately after the run row is created (Task 10).
	InputStorageKey     string   // storage key passed to the storage task ("run-inputs/<runID>")
	InputSize           int      // ciphertext byte size
	InputStoredAt       int64    // unix timestamp the blob was stored (AAD-bound)
	InputRedactedFields []string // dotted paths of any redacted fields
	InputPinned         int      // 1 = pinned (excluded from retention cleanup)

	// Suspend/resume persistence fields (#95). Populated by GetRun only; the
	// list queries omit them (mirrors the input-persistence fields above).
	// Zero values mean "not suspended".
	ResumeState    []byte // opaque task-provided state blob; nil when absent
	ResumeForm     []byte // form schema JSON to render on resume; nil when absent
	ResumeToken    string // unguessable resume handle; empty when absent
	SuspendedAt    int64  // Unix ms the run suspended; 0 when absent
	ResumeDeadline int64  // Unix ms TTL; 0 when no deadline set
	// ResumeParams is a JSON envelope of the original run's fire-time param
	// overrides and chain depth, preserved so the continuation resumes with the
	// same ctx.params and honors the same chain-depth ceiling. nil when absent.
	ResumeParams []byte
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
	tasks   map[string]task.Kinded // was map[string]*task.Spec
	db      db.DB
	logHook func(runID, level, msg string, ts int64)
	logMu   sync.Mutex
}

// New creates an empty Registry backed by the given DB.
func New(database db.DB) *Registry {
	return &Registry{
		tasks: make(map[string]task.Kinded),
		db:    database,
	}
}

// Register upserts any task kind into the registry. *task.Spec satisfies
// task.Kinded, so existing callers passing a *task.Spec keep compiling.
func (r *Registry) Register(k task.Kinded) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tasks[k.TaskID()] = k
	return nil
}

// Unregister removes a task from the registry.
func (r *Registry) Unregister(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tasks, id)
}

// Get returns the *task.Spec for a Task-kind task, or (nil, false) if not
// found OR if the ID names a non-Task kind. Existing consumers only want
// kind: Task, so this filter keeps them unchanged.
func (r *Registry) Get(id string) (*task.Spec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	k, ok := r.tasks[id]
	if !ok {
		return nil, false
	}
	s, ok := k.(*task.Spec)
	return s, ok
}

// GetKinded returns any registered task kind by ID, or (nil, false) if not found.
func (r *Registry) GetKinded(id string) (task.Kinded, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	k, ok := r.tasks[id]
	return k, ok
}

// All returns a snapshot of all kind: Task specs, sorted by ID.
func (r *Registry) All() []*task.Spec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*task.Spec, 0, len(r.tasks))
	for _, k := range r.tasks {
		if s, ok := k.(*task.Spec); ok {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// AllKinded returns a snapshot of every registered task kind, sorted by ID.
func (r *Registry) AllKinded() []task.Kinded {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]task.Kinded, 0, len(r.tasks))
	for _, k := range r.tasks {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TaskID() < out[j].TaskID() })
	return out
}

// StartRun records a new run in sqlite and returns its ID.
func (r *Registry) StartRun(ctx context.Context, taskID, parentRunID string) (string, error) {
	return r.StartRunWithID(ctx, uuid.New().String(), taskID, parentRunID, "", RunKindTask)
}

// StartRunWithID records a new run using a caller-supplied ID.
// Use this when the run ID must be known before execution begins (e.g. async fire).
// kind may be "task" or "pipeline"; empty defaults to "task".
func (r *Registry) StartRunWithID(ctx context.Context, id, taskID, parentRunID, triggerSource, kind string) (string, error) {
	if kind == "" {
		kind = RunKindTask
	}
	now := time.Now().UnixMilli()
	err := r.db.Exec(ctx,
		`INSERT INTO runs (id, task_id, status, started_at, parent_run_id, trigger_source, kind) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, taskID, StatusRunning, now, parentRunID, triggerSource, kind,
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

// FinishRunWithResult atomically updates the run status, finished_at,
// return_value, and structured output columns in a single UPDATE.
// This eliminates the race where a reader sees status=success but an
// empty return_value because FinishRun and SetRunResult were separate writes.
func (r *Registry) FinishRunWithResult(ctx context.Context, runID, status, returnValueJSON, outputContentType, outputContent string) error {
	now := time.Now().UnixMilli()
	return r.db.Exec(ctx,
		`UPDATE runs SET status = ?, finished_at = ?, return_value = ?, output_content_type = ?, output_content = ? WHERE id = ?`,
		status, now, returnValueJSON, outputContentType, outputContent, runID,
	)
}

// SuspendRun records a run as suspended awaiting user input, persisting the
// opaque state/form blobs, the resume token, and the suspend timestamp. It
// deliberately leaves finished_at NULL: suspended is a non-terminal status, so
// the run is neither running (CleanupStaleRuns skips it) nor finished.
//
// A deadline of 0 stores NULL (no TTL). state, form, and resumeParams may be nil.
func (r *Registry) SuspendRun(ctx context.Context, runID string, state, form []byte, token string, suspendedAt, deadline int64, resumeParams []byte) error {
	var deadlineArg any
	if deadline > 0 {
		deadlineArg = deadline
	}
	return r.db.Exec(ctx,
		`UPDATE runs SET status = ?, resume_state = ?, resume_form = ?, resume_token = ?, suspended_at = ?, resume_deadline = ?, resume_params = ? WHERE id = ?`,
		StatusSuspended, state, form, token, suspendedAt, deadlineArg, resumeParams, runID,
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

// runColumns is the SELECT column list shared by GetRun and queryRuns.
// scanRun decodes exactly these columns, in this order. Adding a run column
// means extending this list plus scanRun — nothing else.
const runColumns = `id, task_id, COALESCE(kind, 'task'), status, started_at, finished_at, parent_run_id, trigger_source,
        COALESCE(return_value, ''), COALESCE(output_content_type, ''), COALESCE(output_content, ''),
        COALESCE(fail_reason, ''), COALESCE(run_group, '')`

// runInputColumns are the input-persistence columns GetRun additionally
// selects (appended after runColumns). queryRuns deliberately omits them:
// list endpoints serve the run table and don't surface input metadata, and
// list responses serialize Run directly, so populating these fields there
// would change API output.
const runInputColumns = `,
        COALESCE(input_storage_key, ''), COALESCE(input_size, 0), COALESCE(input_stored_at, 0),
        COALESCE(input_redacted_fields, ''), COALESCE(input_pinned, 0)`

// runResumeColumns are the suspend/resume columns GetRun additionally selects
// (appended after runInputColumns). Like the input columns, queryRuns omits
// them: list responses serialize Run directly and don't surface resume state.
// The BLOB columns are scanned as-is (NULL → nil []byte); the scalars use
// COALESCE so a NULL reads back as the zero value.
const runResumeColumns = `,
        resume_state, resume_form, COALESCE(resume_token, ''),
        COALESCE(suspended_at, 0), COALESCE(resume_deadline, 0), resume_params`

// scanRun decodes one row selected with runColumns (plus runInputColumns when
// withInput is true, plus runResumeColumns when withResume is true) into a Run,
// including the shared post-processing (trigger source, millisecond timestamps,
// nullable parent, redacted-fields JSON). rows.Next() must already have
// returned true.
func scanRun(rows db.Scanner, withInput, withResume bool) (*Run, error) {
	run := &Run{}
	var startedMs int64
	var finishedMs *int64
	var parentID *string
	var tsStr string
	var redactedFieldsJSON string
	dest := []any{
		&run.ID, &run.TaskID, &run.Kind, &run.Status, &startedMs, &finishedMs, &parentID,
		&tsStr, &run.ReturnValue, &run.OutputContentType, &run.OutputContent,
		&run.FailureReason, &run.Group,
	}
	if withInput {
		dest = append(dest,
			&run.InputStorageKey, &run.InputSize, &run.InputStoredAt, &redactedFieldsJSON, &run.InputPinned)
	}
	if withResume {
		dest = append(dest,
			&run.ResumeState, &run.ResumeForm, &run.ResumeToken, &run.SuspendedAt, &run.ResumeDeadline,
			&run.ResumeParams)
	}
	if err := rows.Scan(dest...); err != nil {
		return nil, err
	}
	run.TriggerSource = TriggerSource(tsStr)
	run.StartedAt = time.UnixMilli(startedMs)
	if finishedMs != nil {
		t := time.UnixMilli(*finishedMs)
		run.FinishedAt = &t
	}
	if parentID != nil {
		run.ParentRunID = *parentID
	}
	if redactedFieldsJSON != "" && redactedFieldsJSON != "null" {
		// input_redacted_fields is daemon-written json.Marshal([]string); a
		// malformed value signals data corruption, so surface it rather than
		// silently returning empty (potentially misleading) redaction metadata.
		if err := json.Unmarshal([]byte(redactedFieldsJSON), &run.InputRedactedFields); err != nil {
			return nil, fmt.Errorf("decode input_redacted_fields for run %s: %w", run.ID, err)
		}
	}
	return run, nil
}

// GetRun fetches a run record by ID.
func (r *Registry) GetRun(ctx context.Context, runID string) (*Run, error) {
	var run *Run
	err := r.db.Query(ctx,
		`SELECT `+runColumns+runInputColumns+runResumeColumns+` FROM runs WHERE id = ?`,
		[]any{runID},
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
		return nil, fmt.Errorf("get run %s: %w", runID, err)
	}
	if run == nil {
		return nil, fmt.Errorf("run %s: %w", runID, ErrRunNotFound)
	}
	return run, nil
}

// SetRunGroup labels a run with a free-text group string (#116). Last write
// wins; intended to be called by the running task itself via dicode.set_group().
func (r *Registry) SetRunGroup(ctx context.Context, runID, group string) error {
	return r.db.Exec(ctx, `UPDATE runs SET run_group = ? WHERE id = ?`, group, runID)
}

// SetRunInput updates the runs row with the persistence handle after the input
// blob has been stored. Called by the trigger engine immediately after Persist
// succeeds.
func (r *Registry) SetRunInput(ctx context.Context, runID, storageKey string, size int, storedAt int64, redactedFields []string) error {
	rfJSON, err := json.Marshal(redactedFields)
	if err != nil {
		return fmt.Errorf("marshal redacted_fields: %w", err)
	}
	if err := r.db.Exec(ctx,
		`UPDATE runs SET input_storage_key = ?, input_size = ?, input_stored_at = ?, input_redacted_fields = ?
		 WHERE id = ?`,
		storageKey, size, storedAt, string(rfJSON), runID,
	); err != nil {
		return fmt.Errorf("update runs: %w", err)
	}
	return nil
}

// ListRuns returns the most recent runs for a task (newest first).
func (r *Registry) ListRuns(ctx context.Context, taskID string, limit int) ([]*Run, error) {
	return r.queryRuns(ctx,
		`WHERE task_id = ? ORDER BY started_at DESC LIMIT ?`,
		[]any{taskID, limit})
}

// ListChildren returns runs whose parent_run_id == parentID, newest first (#116).
func (r *Registry) ListChildren(ctx context.Context, parentRunID string, limit int) ([]*Run, error) {
	return r.queryRuns(ctx,
		`WHERE parent_run_id = ? ORDER BY started_at DESC LIMIT ?`,
		[]any{parentRunID, limit})
}

// ListByGroup returns runs for a task with the given group label, newest first (#116).
// taskID scopes the query because group labels are task-local — the same label
// from different tasks must not collide.
func (r *Registry) ListByGroup(ctx context.Context, taskID, group string, limit int) ([]*Run, error) {
	return r.queryRuns(ctx,
		`WHERE task_id = ? AND run_group = ? ORDER BY started_at DESC LIMIT ?`,
		[]any{taskID, group, limit})
}

// queryRuns runs a `SELECT … FROM runs <whereAndLimitClause>` and decodes rows
// into Run structs. The clause must include any ORDER BY / LIMIT.
func (r *Registry) queryRuns(ctx context.Context, whereAndLimit string, args []any) ([]*Run, error) {
	var runs []*Run
	err := r.db.Query(ctx,
		`SELECT `+runColumns+` FROM runs `+whereAndLimit,
		args,
		func(rows db.Scanner) error {
			for rows.Next() {
				run, err := scanRun(rows, false, false)
				if err != nil {
					return err
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
//
// The SELECT and UPDATE run inside a transaction so a run that starts between
// the two statements cannot be erroneously cancelled.
func (r *Registry) CleanupStaleRuns(ctx context.Context) ([]string, error) {
	var taskIDs []string
	err := r.db.Tx(ctx, func(tx db.DB) error {
		taskIDs = nil // reset on retry
		if err := tx.Query(ctx,
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
		); err != nil {
			return fmt.Errorf("query stale runs: %w", err)
		}
		if len(taskIDs) == 0 {
			return nil
		}
		now := time.Now().UnixMilli()
		if err := tx.Exec(ctx,
			`UPDATE runs SET status = ?, finished_at = ? WHERE status = ?`,
			StatusCancelled, now, StatusRunning,
		); err != nil {
			return fmt.Errorf("cancel stale runs: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
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

// ── Run-input retention management ───────────────────────────────────────────

// ExpiredInput identifies a run whose persisted input is past retention.
type ExpiredInput struct {
	RunID      string `json:"runID"`
	StorageKey string `json:"storageKey"`
	StoredAt   int64  `json:"storedAt"`
}

// ListExpiredInputs returns rows whose input_stored_at < beforeUnix and which
// aren't pinned. Used by the run-inputs-cleanup buildin (#233 Task 12).
func (r *Registry) ListExpiredInputs(ctx context.Context, beforeUnix int64) ([]ExpiredInput, error) {
	var out []ExpiredInput
	err := r.db.Query(ctx,
		`SELECT id, input_storage_key, input_stored_at FROM runs
		 WHERE input_storage_key IS NOT NULL
		   AND input_storage_key != ''
		   AND input_stored_at < ?
		   AND input_pinned = 0`,
		[]any{beforeUnix},
		func(rows db.Scanner) error {
			for rows.Next() {
				var e ExpiredInput
				if err := rows.Scan(&e.RunID, &e.StorageKey, &e.StoredAt); err != nil {
					return err
				}
				out = append(out, e)
			}
			return nil
		},
	)
	return out, err
}

// ClearRunInput nulls the input_storage_key/size/stored_at/redacted_fields
// columns on a row. The caller is responsible for deleting the actual blob
// from the storage task (typically via InputStore.Delete) BEFORE calling
// this — the column clear is the authoritative "input gone" signal.
func (r *Registry) ClearRunInput(ctx context.Context, runID string) error {
	return r.db.Exec(ctx,
		`UPDATE runs SET input_storage_key = NULL, input_size = NULL,
		                  input_stored_at = NULL, input_redacted_fields = NULL
		 WHERE id = ?`, runID)
}

// PinRunInput sets input_pinned = 1 on the given run.
func (r *Registry) PinRunInput(ctx context.Context, runID string) error {
	return r.db.Exec(ctx, `UPDATE runs SET input_pinned = 1 WHERE id = ?`, runID)
}

// UnpinRunInput sets input_pinned = 0 on the given run.
func (r *Registry) UnpinRunInput(ctx context.Context, runID string) error {
	return r.db.Exec(ctx, `UPDATE runs SET input_pinned = 0 WHERE id = ?`, runID)
}

// SweepStalePins clears input_pinned on any row whose pin is no longer
// load-bearing — i.e., the run's status is not "running" anymore. Returns
// the number of rows cleared.
//
// Called at engine startup to recover from daemons that crashed mid-fix
// before unpinning. A pinned + finished row would otherwise prevent the
// retention sweep from ever collecting that input blob.
//
// The COUNT and UPDATE run inside a transaction so a run that completes
// between the two statements cannot be missed or double-cleared.
func (r *Registry) SweepStalePins(ctx context.Context) (int, error) {
	// SQLite doesn't return RowsAffected through the DB.Exec wrapper without
	// an extra round-trip. Count first, then update — one extra query is
	// fine at startup.
	var n int
	err := r.db.Tx(ctx, func(tx db.DB) error {
		n = 0 // reset on retry
		if err := tx.Query(ctx,
			`SELECT COUNT(*) FROM runs WHERE input_pinned = 1 AND status != ?`,
			[]any{StatusRunning},
			func(rows db.Scanner) error {
				if rows.Next() {
					return rows.Scan(&n)
				}
				return nil
			},
		); err != nil {
			return err
		}
		if n == 0 {
			return nil
		}
		return tx.Exec(ctx,
			`UPDATE runs SET input_pinned = 0 WHERE input_pinned = 1 AND status != ?`,
			StatusRunning,
		)
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}
