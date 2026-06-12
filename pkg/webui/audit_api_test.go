package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dicode/dicode/pkg/audit"
	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
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
