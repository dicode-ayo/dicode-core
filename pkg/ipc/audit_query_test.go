package ipc

import (
	"context"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/audit"
	"github.com/dicode/dicode/pkg/task"
)

// seedAudit appends n run_triggered events with spread timestamps so the
// (ts, id) ordering is deterministic.
func seedAudit(t *testing.T, e *testEnv, n int) {
	t.Helper()
	store := audit.NewStore(e.db)
	for i := 0; i < n; i++ {
		ev := audit.Event{
			EventType: audit.EventRunTriggered,
			ActorKind: "cron",
			TargetID:  "ns/task",
			TS:        time.Date(2026, 6, 1, 12, 0, i, 0, time.UTC),
			Allowed:   true,
		}
		if err := store.Append(context.Background(), ev); err != nil {
			t.Fatalf("seed append[%d]: %v", i, err)
		}
	}
}

// TestServer_Dicode_AuditQuery_Denied confirms an ungated task cannot read
// the audit trail — the capability is never granted ambiently.
func TestServer_Dicode_AuditQuery_Denied(t *testing.T) {
	e := newTestEnv(t)
	seedAudit(t, e, 3)
	conn, _ := e.start(t, nil, nil) // no permissions.dicode → no audit.query cap

	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.audit.query"})
	resp := recvMsg(t, conn)
	if resp["error"] == nil {
		t.Fatalf("expected permission denied for dicode.audit.query without audit_query cap, got %v", resp)
	}
}

// TestServer_Dicode_AuditQuery_Allowed confirms a task with audit_query: true
// receives events and a next_cursor.
func TestServer_Dicode_AuditQuery_Allowed(t *testing.T) {
	e := newTestEnv(t)
	seedAudit(t, e, 3)
	spec := specWithDicode("caller", &task.DicodePermissions{AuditQuery: true})
	conn, _ := e.startWithSpec(t, nil, nil, spec, nil)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.audit.query", "order": "asc"})
	resp := recvMsg(t, conn)
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected object result, got %T", resp["result"])
	}
	events, ok := result["events"].([]any)
	if !ok || len(events) != 3 {
		t.Fatalf("expected 3 events, got %v", result["events"])
	}
	if result["next_cursor"] == "" || result["next_cursor"] == nil {
		t.Errorf("expected non-empty next_cursor, got %v", result["next_cursor"])
	}
}

// TestServer_Dicode_AuditQuery_CursorPaging walks the trail forward via the
// returned next_cursor and confirms no overlap and full coverage.
func TestServer_Dicode_AuditQuery_CursorPaging(t *testing.T) {
	e := newTestEnv(t)
	seedAudit(t, e, 5)
	spec := specWithDicode("caller", &task.DicodePermissions{AuditQuery: true})
	conn, _ := e.startWithSpec(t, nil, nil, spec, nil)

	page := func(after string) (ids []string, next string) {
		sendMsg(t, conn, map[string]any{
			"id": "1", "method": "dicode.audit.query",
			"order": "asc", "limit": 2, "after": after,
		})
		resp := recvMsg(t, conn)
		if resp["error"] != nil {
			t.Fatalf("page error: %v", resp["error"])
		}
		result := resp["result"].(map[string]any)
		for _, raw := range result["events"].([]any) {
			ids = append(ids, raw.(map[string]any)["id"].(string))
		}
		next, _ = result["next_cursor"].(string)
		return ids, next
	}

	p1, c1 := page("")
	p2, c2 := page(c1)
	p3, _ := page(c2)

	if len(p1) != 2 || len(p2) != 2 || len(p3) != 1 {
		t.Fatalf("page sizes: %d, %d, %d; want 2, 2, 1", len(p1), len(p2), len(p3))
	}
	seen := map[string]bool{}
	for _, id := range append(append(p1, p2...), p3...) {
		if seen[id] {
			t.Errorf("duplicate id across pages: %s", id)
		}
		seen[id] = true
	}
	if len(seen) != 5 {
		t.Errorf("expected 5 distinct events across pages, got %d", len(seen))
	}
}

// TestServer_Dicode_AuditQuery_BadOrder rejects an invalid order value.
func TestServer_Dicode_AuditQuery_BadOrder(t *testing.T) {
	e := newTestEnv(t)
	spec := specWithDicode("caller", &task.DicodePermissions{AuditQuery: true})
	conn, _ := e.startWithSpec(t, nil, nil, spec, nil)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.audit.query", "order": "sideways"})
	resp := recvMsg(t, conn)
	if resp["error"] == nil {
		t.Errorf("expected error for invalid order, got %v", resp)
	}
}
