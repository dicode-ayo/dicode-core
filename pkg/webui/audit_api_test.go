package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/audit"
	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/trigger"
	"go.uber.org/zap"
)

// newAuditTestServer builds a Server without the deno runtime so these
// tests run on machines where the deno binary is unavailable.
func newAuditTestServer(t *testing.T, cfg *config.Config) *Server {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	srv, err := New(cfg.Server.Port, reg, eng, cfg, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

func TestAudit_DeniedAuthEmitsEvent(t *testing.T) {
	cfg := &config.Config{Server: config.ServerConfig{Port: 8080, Auth: true, BcryptCost: 4}}
	srv := newAuditTestServer(t, cfg)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	req.RemoteAddr = "203.0.113.9:50000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}

	events, err := srv.audit.Query(context.Background(), audit.Filter{EventType: audit.EventDenied})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d denied events, want 1", len(events))
	}
	ev := events[0]
	if ev.Allowed {
		t.Error("denied event must have allowed=false")
	}
	if ev.ActorKind != "ip" || ev.ActorID != "203.0.113.9" {
		t.Errorf("actor: %s/%s", ev.ActorKind, ev.ActorID)
	}
	if ev.TargetKind != "endpoint" || ev.TargetID != "GET /api/tasks" {
		t.Errorf("target: %s/%s", ev.TargetKind, ev.TargetID)
	}
}

func TestAudit_NoEventWhenAuthDisabled(t *testing.T) {
	cfg := &config.Config{Server: config.ServerConfig{Port: 8080}}
	srv := newAuditTestServer(t, cfg)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with auth disabled, got %d", w.Code)
	}
	events, err := srv.audit.Query(context.Background(), audit.Filter{EventType: audit.EventDenied})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected no denied events with auth off, got %d", len(events))
	}
}

// TestAPIAudit_ManualRunRecordsActor verifies the manual-run API path
// (POST /api/tasks/{id}/run) stamps the operator principal — the session's
// client IP — into the run_triggered audit event's actor_id (#45).
func TestAPIAudit_ManualRunRecordsActor(t *testing.T) {
	cfg := &config.Config{Server: config.ServerConfig{Port: 8080}} // auth off → endpoint reachable
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	eng.SetDB(d) // wire the engine's audit store, as daemon.go does in production
	srv, err := New(cfg.Server.Port, reg, eng, cfg, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := reg.Register(&task.Spec{
		ID:      "actor-task",
		Name:    "actor-task",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true},
		Timeout: 5 * time.Second,
		Enabled: true,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/tasks/actor-task/run", nil)
	req.RemoteAddr = "203.0.113.9:50000"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("run: status %d: %s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}

	// Wait for the (executor-less, immediately failing) run to finish so its
	// background goroutine cannot race the test's DB teardown.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := eng.WaitRun(ctx, body["runId"]); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}

	events, err := srv.audit.Query(ctx, audit.Filter{EventType: audit.EventRunTriggered})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("got %d run_triggered events, want 1", len(events))
	}
	ev := events[0]
	if ev.ActorKind != "manual" || ev.ActorID != "203.0.113.9" {
		t.Errorf("actor: %s/%s, want manual/203.0.113.9", ev.ActorKind, ev.ActorID)
	}
	if ev.TargetKind != "task" || ev.TargetID != "actor-task" {
		t.Errorf("target: %s/%s", ev.TargetKind, ev.TargetID)
	}
	if ev.RunID != body["runId"] {
		t.Errorf("run_id: %s, want %s", ev.RunID, body["runId"])
	}
}

// TestAPIAudit_RequiresAuth verifies GET /api/audit sits behind requireAuth:
// with server.auth enabled, an unauthenticated request (no session, no
// cookie) must be rejected with 401 instead of leaking audit events.
func TestAPIAudit_RequiresAuth(t *testing.T) {
	cfg := &config.Config{Server: config.ServerConfig{Port: 8080, Auth: true, BcryptCost: 4}}
	srv := newAuditTestServer(t, cfg)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/audit", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAPIAudit_QueryAndFilter(t *testing.T) {
	cfg := &config.Config{Server: config.ServerConfig{Port: 8080}} // auth off → endpoint reachable
	srv := newAuditTestServer(t, cfg)
	h := srv.Handler()
	ctx := context.Background()

	seed := []audit.Event{
		{EventType: audit.EventRunTriggered, ActorKind: "cron", TargetKind: "task", TargetID: "ns/a", RunID: "r1", Allowed: true},
		{EventType: audit.EventTaskCalled, ActorKind: "task", ActorID: "ns/caller", TargetKind: "task", TargetID: "ns/b", Allowed: true},
		{EventType: audit.EventTaskCalled, ActorKind: "task", ActorID: "ns/caller", TargetKind: "task", TargetID: "ns/a", Allowed: false, Reason: "denied"},
	}
	for i, ev := range seed {
		if err := srv.audit.Append(ctx, ev); err != nil {
			t.Fatalf("seed[%d]: %v", i, err)
		}
	}

	get := func(url string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, url, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d: %s", url, w.Code, w.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("GET %s: bad JSON: %v", url, err)
		}
		return body
	}

	// Unfiltered.
	body := get("/api/audit")
	if int(body["count"].(float64)) != 3 {
		t.Errorf("unfiltered count: %v", body["count"])
	}

	// task_id filter.
	body = get("/api/audit?task_id=ns/a")
	if int(body["count"].(float64)) != 2 {
		t.Errorf("task_id filter count: %v", body["count"])
	}

	// actor filter.
	body = get("/api/audit?actor=ns/caller")
	if int(body["count"].(float64)) != 2 {
		t.Errorf("actor filter count: %v", body["count"])
	}

	// combined + event_type.
	body = get("/api/audit?actor=ns/caller&task_id=ns/a&event_type=task_called")
	if int(body["count"].(float64)) != 1 {
		t.Fatalf("combined filter count: %v", body["count"])
	}
	events := body["events"].([]any)
	first := events[0].(map[string]any)
	if first["reason"] != "denied" || first["allowed"] != false {
		t.Errorf("combined filter event: %v", first)
	}

	// limit pagination.
	body = get("/api/audit?limit=1")
	if int(body["count"].(float64)) != 1 {
		t.Errorf("limit=1 count: %v", body["count"])
	}
	body = get("/api/audit?limit=1&offset=2")
	if int(body["count"].(float64)) != 1 {
		t.Errorf("limit=1 offset=2 count: %v", body["count"])
	}
}

// TestAPIAudit_DescDefaultUnchanged is the #415 regression guard: with no
// order/after params the response is newest-first, exactly as #45 shipped.
func TestAPIAudit_DescDefaultUnchanged(t *testing.T) {
	cfg := &config.Config{Server: config.ServerConfig{Port: 8080}}
	srv := newAuditTestServer(t, cfg)
	h := srv.Handler()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := srv.audit.Append(ctx, audit.Event{
			EventType: audit.EventRunTriggered, TargetID: "t",
			TS: time.Date(2026, 6, 1, 12, 0, i, 0, time.UTC), Allowed: true,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/audit", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	events := body["events"].([]any)
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	// Newest first: the latest ts is element 0.
	first := events[0].(map[string]any)["ts"].(string)
	last := events[2].(map[string]any)["ts"].(string)
	if first <= last {
		t.Errorf("expected newest-first (desc) by default: first=%s last=%s", first, last)
	}
}

// TestAPIAudit_CursorForward walks /api/audit forward with order=asc + after=
// (next_cursor) and confirms no overlap and full coverage.
func TestAPIAudit_CursorForward(t *testing.T) {
	cfg := &config.Config{Server: config.ServerConfig{Port: 8080}}
	srv := newAuditTestServer(t, cfg)
	h := srv.Handler()
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := srv.audit.Append(ctx, audit.Event{
			EventType: audit.EventRunTriggered, TargetID: "t",
			TS: time.Date(2026, 6, 1, 12, 0, i, 0, time.UTC), Allowed: true,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	get := func(url string) map[string]any {
		req := httptest.NewRequest(http.MethodGet, url, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d", url, w.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("bad JSON: %v", err)
		}
		return body
	}

	seen := map[string]bool{}
	cursor := ""
	for {
		url := "/api/audit?order=asc&limit=2"
		if cursor != "" {
			url += "&after=" + cursor
		}
		body := get(url)
		events := body["events"].([]any)
		if len(events) == 0 {
			break
		}
		for _, raw := range events {
			id := raw.(map[string]any)["id"].(string)
			if seen[id] {
				t.Errorf("duplicate id %s across cursor pages", id)
			}
			seen[id] = true
		}
		cursor = body["next_cursor"].(string)
	}
	if len(seen) != 5 {
		t.Errorf("expected 5 distinct events, got %d", len(seen))
	}
}

// TestAPIAudit_BadParams rejects malformed order/cursor with 400.
func TestAPIAudit_BadParams(t *testing.T) {
	cfg := &config.Config{Server: config.ServerConfig{Port: 8080}}
	srv := newAuditTestServer(t, cfg)
	h := srv.Handler()

	for _, url := range []string{"/api/audit?order=bogus", "/api/audit?after=%21%21not-base64"} {
		req := httptest.NewRequest(http.MethodGet, url, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("GET %s: expected 400, got %d", url, w.Code)
		}
	}
}
