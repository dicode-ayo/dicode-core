// Package audit provides a structured audit log for security-sensitive
// operations (issue #45). Events are appended to the `audit_log` SQLite
// table (created by the standard pkg/db migration) at four boundaries:
//
//   - run_triggered — the trigger engine starts a run (cron, webhook,
//     manual, chain, daemon, …)
//   - task_called   — a task invokes dicode.run_task over IPC
//   - mcp_called    — an MCP tools/call invocation (dicode.run_task with
//     MCP context) or an outbound mcp.call to an external MCP server
//   - denied        — the web UI rejects an unauthenticated request
//
// The store is intentionally dependency-light: it only needs a db.DB
// handle. All write paths are best-effort — a failed audit insert must
// never break the operation being audited — and every method is safe to
// call on a nil *Store (no-op), so callers don't need nil guards.
package audit

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/google/uuid"
)

// Event types emitted at the security boundaries.
const (
	EventRunTriggered = "run_triggered"
	EventTaskCalled   = "task_called"
	EventMCPCalled    = "mcp_called"
	EventDenied       = "denied"
)

// tsLayout is the storage format for the ts column. It is prefix-compatible
// with SQLite's CURRENT_TIMESTAMP / datetime() output ("YYYY-MM-DD HH:MM:SS")
// so lexicographic comparisons in WHERE/ORDER BY clauses remain correct.
const tsLayout = "2006-01-02 15:04:05.000"

// Event is a single audit-log row.
type Event struct {
	ID         string    `json:"id"`
	TS         time.Time `json:"ts"`
	EventType  string    `json:"event_type"`
	ActorKind  string    `json:"actor_kind"`       // "task" | "ip" | trigger source ("cron", "webhook", …)
	ActorID    string    `json:"actor_id"`         // task id, client IP, parent run id, …
	TargetKind string    `json:"target_kind"`      // "task" | "mcp" | "endpoint"
	TargetID   string    `json:"target_id"`        // task id, mcp name/tool, HTTP "METHOD /path"
	Params     string    `json:"params,omitempty"` // sanitized JSON (see SanitizeParams) — never raw secrets
	RunID      string    `json:"run_id,omitempty"` // associated run, when known
	Allowed    bool      `json:"allowed"`
	Reason     string    `json:"reason,omitempty"` // denial reason or context note
}

// Filter selects events for Query. Zero values mean "no constraint".
type Filter struct {
	TaskID    string // matches target_id (the task/tool/endpoint acted upon)
	Actor     string // matches actor_id
	EventType string // matches event_type
	Limit     int    // default defaultQueryLimit, capped at maxQueryLimit
	Offset    int    // pagination offset
}

const (
	defaultQueryLimit = 100
	maxQueryLimit     = 1000
)

// Store appends and queries audit events. Construct with NewStore. All
// methods are nil-receiver safe so callers can wire it optionally.
type Store struct {
	db db.DB
}

// NewStore returns a Store backed by the given database. Returns nil when
// database is nil so the nil-safe no-op behaviour kicks in automatically.
func NewStore(database db.DB) *Store {
	if database == nil {
		return nil
	}
	return &Store{db: database}
}

// Append inserts one event. ID and TS are filled in when empty. Returns
// an error only for real DB failures; nil receiver is a silent no-op.
func (s *Store) Append(ctx context.Context, ev Event) error {
	if s == nil || s.db == nil {
		return nil
	}
	if ev.ID == "" {
		ev.ID = uuid.New().String()
	}
	if ev.TS.IsZero() {
		ev.TS = time.Now().UTC()
	}
	allowed := 0
	if ev.Allowed {
		allowed = 1
	}
	return s.db.Exec(ctx,
		`INSERT INTO audit_log
		   (id, ts, event_type, actor_kind, actor_id, target_kind, target_id, params, run_id, allowed, reason)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.ID, ev.TS.UTC().Format(tsLayout), ev.EventType,
		ev.ActorKind, ev.ActorID, ev.TargetKind, ev.TargetID,
		ev.Params, ev.RunID, allowed, ev.Reason,
	)
}

// Emit is the fire-and-forget variant of Append used on hot paths where a
// failed audit insert must never break the audited operation. The error is
// intentionally discarded — the audited operation already has its own
// logging, and Append failures are limited to DB-level problems that the
// daemon surfaces elsewhere.
func (s *Store) Emit(ctx context.Context, ev Event) {
	_ = s.Append(ctx, ev)
}

// Query returns events matching the filter, newest first.
func (s *Store) Query(ctx context.Context, f Filter) ([]Event, error) {
	if s == nil || s.db == nil {
		return []Event{}, nil
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultQueryLimit
	}
	if limit > maxQueryLimit {
		limit = maxQueryLimit
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	var where []string
	var args []any
	if f.TaskID != "" {
		where = append(where, "target_id = ?")
		args = append(args, f.TaskID)
	}
	if f.Actor != "" {
		where = append(where, "actor_id = ?")
		args = append(args, f.Actor)
	}
	if f.EventType != "" {
		where = append(where, "event_type = ?")
		args = append(args, f.EventType)
	}
	q := `SELECT id, ts, event_type, actor_kind, actor_id, target_kind, target_id, params, run_id, allowed, reason
	      FROM audit_log`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY ts DESC, id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	events := []Event{}
	err := s.db.Query(ctx, q, args, func(rows db.Scanner) error {
		for rows.Next() {
			var ev Event
			var ts string
			var allowed int
			if err := rows.Scan(&ev.ID, &ts, &ev.EventType, &ev.ActorKind, &ev.ActorID,
				&ev.TargetKind, &ev.TargetID, &ev.Params, &ev.RunID, &allowed, &ev.Reason); err != nil {
				return err
			}
			ev.TS = parseTS(ts)
			ev.Allowed = allowed != 0
			events = append(events, ev)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("audit query: %w", err)
	}
	return events, nil
}

// parseTS decodes a stored ts column value. Accepts both the fractional
// layout this package writes and SQLite's plain CURRENT_TIMESTAMP format
// (rows inserted by hand or by future writers).
func parseTS(s string) time.Time {
	for _, layout := range []string{tsLayout, "2006-01-02 15:04:05", time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// Prune deletes events older than retentionDays. retentionDays <= 0 means
// retention is disabled and Prune is a guaranteed no-op — it can never
// wipe the table by accident when the config value is 0/unset.
func (s *Store) Prune(ctx context.Context, retentionDays int) error {
	if s == nil || s.db == nil || retentionDays <= 0 {
		return nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays).Format(tsLayout)
	return s.db.Exec(ctx, `DELETE FROM audit_log WHERE ts < ?`, cutoff)
}
