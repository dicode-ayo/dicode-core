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
	"encoding/base64"
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
	Offset    int    // pagination offset (ignored when After is set)

	// After is an exclusive (ts, id) cursor. When non-zero, Query returns
	// only rows strictly after it in the chosen order, and Offset is
	// ignored — cursor and offset paging are mutually exclusive. The (ts,
	// id) tuple is used rather than ts alone because ts is not unique;
	// the id tiebreak makes resumption stable under a concurrent writer.
	After Cursor

	// Ascending requests oldest-first ordering. The default (false) keeps
	// the newest-first contract the UI/API relies on. A log shipper sets
	// this together with After to walk the trail forward chronologically.
	Ascending bool
}

// Cursor is an opaque (ts, id) position in the audit log. The zero value
// means "no cursor". Encode/Decode round-trip it through the opaque base64
// form used on the wire and in API responses.
type Cursor struct {
	TS time.Time
	ID string
}

// IsZero reports whether the cursor is unset.
func (c Cursor) IsZero() bool { return c.ID == "" && c.TS.IsZero() }

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

// Query returns events matching the filter. Newest-first by default;
// oldest-first when Filter.Ascending is set. When Filter.After is set the
// result starts strictly after that (ts, id) cursor (Offset is ignored).
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
	if !f.After.IsZero() {
		// Lexicographic (ts, id) tuple comparison. ts is stored in a
		// lexicographically-sortable layout (tsLayout), so a row-wise
		// tuple comparison via the (ts > c) OR (ts = c AND id > c) form is
		// equivalent to ordering on the (ts, id) pair. Ascending walks
		// forward (>), descending walks backward (<).
		cmp := ">"
		if !f.Ascending {
			cmp = "<"
		}
		where = append(where, fmt.Sprintf("(ts %s ? OR (ts = ? AND id %s ?))", cmp, cmp))
		cts := f.After.TS.UTC().Format(tsLayout)
		args = append(args, cts, cts, f.After.ID)
	}
	q := `SELECT id, ts, event_type, actor_kind, actor_id, target_kind, target_id, params, run_id, allowed, reason
	      FROM audit_log`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	if f.Ascending {
		q += " ORDER BY ts ASC, id ASC"
	} else {
		q += " ORDER BY ts DESC, id DESC"
	}
	// Offset and cursor are mutually exclusive: a cursor already encodes the
	// resume position, so applying a stale offset on top would skip rows.
	if f.After.IsZero() {
		q += " LIMIT ? OFFSET ?"
		args = append(args, limit, offset)
	} else {
		q += " LIMIT ?"
		args = append(args, limit)
	}

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

// CursorOf returns the opaque resume cursor for an event — the (ts, id)
// position a consumer passes back as Filter.After to continue after it.
func CursorOf(ev Event) Cursor { return Cursor{TS: ev.TS, ID: ev.ID} }

// EncodeCursor renders a cursor as an opaque, URL-safe token. The zero
// cursor encodes to "" so an empty cursor round-trips to empty.
func EncodeCursor(c Cursor) string {
	if c.IsZero() {
		return ""
	}
	raw := c.TS.UTC().Format(tsLayout) + "|" + c.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor parses a token produced by EncodeCursor. An empty token
// decodes to the zero cursor. A malformed token is an error so a consumer
// learns its saved position is unusable rather than silently restarting.
func DecodeCursor(tok string) (Cursor, error) {
	if tok == "" {
		return Cursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return Cursor{}, fmt.Errorf("audit: invalid cursor encoding: %w", err)
	}
	tsStr, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return Cursor{}, fmt.Errorf("audit: malformed cursor %q", tok)
	}
	ts := parseTS(tsStr)
	if ts.IsZero() {
		return Cursor{}, fmt.Errorf("audit: cursor has unparseable timestamp")
	}
	return Cursor{TS: ts, ID: id}, nil
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
