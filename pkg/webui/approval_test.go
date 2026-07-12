package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/dicode/dicode/pkg/approval"
	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/trigger"
	"go.uber.org/zap"
)

// fakeApprovalGate implements ApprovalGate over an in-memory pending map.
type fakeApprovalGate struct {
	mu       sync.Mutex
	pending  map[string]string // task id → observed hash
	approved []string
}

func newFakeGate() *fakeApprovalGate {
	return &fakeApprovalGate{pending: map[string]string{}}
}

func (g *fakeApprovalGate) IsPending(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.pending[id]
	return ok
}

func (g *fakeApprovalGate) PendingHash(id string) (string, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	h, ok := g.pending[id]
	return h, ok
}

func (g *fakeApprovalGate) Approve(id string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.pending[id]; !ok {
		return fmt.Errorf("task %q is not pending approval", id)
	}
	delete(g.pending, id)
	g.approved = append(g.approved, id)
	return nil
}

func (g *fakeApprovalGate) ApproveIfHash(id, hash string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	cur, ok := g.pending[id]
	if !ok {
		return fmt.Errorf("task %q is not pending approval", id)
	}
	if hash == "" || cur != hash {
		return fmt.Errorf("task %q changed since the approval was issued", id)
	}
	delete(g.pending, id)
	g.approved = append(g.approved, id)
	return nil
}

// newApprovalTestServer builds a deno-free Server (no runtime needed for the
// approval surfaces) with an optional auth wall, plus the backing db handle
// so token rows can be manipulated directly.
func newApprovalTestServer(t *testing.T, auth bool) (*Server, *registry.Registry, db.DB) {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	cfg := &config.Config{Server: config.ServerConfig{Port: 8080, Auth: auth, Secret: "hunter2"}}
	srv, err := New(8080, reg, eng, cfg, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv, reg, d
}

func registerMinimalTask(t *testing.T, reg *registry.Registry, id string) {
	t.Helper()
	if err := reg.Register(&task.Spec{ID: id, Name: id, Trigger: task.TriggerConfig{Manual: true}, Enabled: true}); err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
}

// ── Pending visibility ───────────────────────────────────────────────────────

func TestAPI_ListTasks_PendingApprovalFlag(t *testing.T) {
	srv, reg, _ := newApprovalTestServer(t, false)
	registerMinimalTask(t, reg, "repo/pending-task")
	registerMinimalTask(t, reg, "repo/approved-task")

	gate := newFakeGate()
	gate.pending["repo/pending-task"] = "hash-1"
	srv.SetApprovalGate(gate)

	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var items []map[string]any
	if err := json.NewDecoder(w.Body).Decode(&items); err != nil {
		t.Fatalf("decode: %v", err)
	}
	flags := map[string]bool{}
	for _, it := range items {
		id, _ := it["id"].(string)
		flags[id], _ = it["pending_approval"].(bool)
	}
	if !flags["repo/pending-task"] {
		t.Error("pending task must carry pending_approval: true")
	}
	if flags["repo/approved-task"] {
		t.Error("non-pending task must not carry pending_approval")
	}
}

func TestAPI_GetTask_PendingApprovalFlag(t *testing.T) {
	srv, reg, _ := newApprovalTestServer(t, false)
	registerMinimalTask(t, reg, "repo/pending-task")
	gate := newFakeGate()
	gate.pending["repo/pending-task"] = "hash-1"
	srv.SetApprovalGate(gate)

	req := httptest.NewRequest(http.MethodGet, "/api/tasks/repo%2Fpending-task", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var detail map[string]any
	if err := json.NewDecoder(w.Body).Decode(&detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, _ := detail["pending_approval"].(bool); !got {
		t.Errorf("detail pending_approval = %v, want true", detail["pending_approval"])
	}
}

// ── POST /api/tasks/{id}/approve ─────────────────────────────────────────────

func TestAPI_ApproveTask_ApprovesPending(t *testing.T) {
	srv, reg, _ := newApprovalTestServer(t, false)
	registerMinimalTask(t, reg, "repo/pending-task")
	gate := newFakeGate()
	gate.pending["repo/pending-task"] = "hash-1"
	srv.SetApprovalGate(gate)

	req := httptest.NewRequest(http.MethodPost, "/api/tasks/repo%2Fpending-task/approve", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if gate.IsPending("repo/pending-task") {
		t.Error("task still pending after approve")
	}
	if len(gate.approved) != 1 || gate.approved[0] != "repo/pending-task" {
		t.Errorf("approved = %v", gate.approved)
	}
}

func TestAPI_ApproveTask_NotPending409(t *testing.T) {
	srv, reg, _ := newApprovalTestServer(t, false)
	registerMinimalTask(t, reg, "repo/normal-task")
	srv.SetApprovalGate(newFakeGate())

	req := httptest.NewRequest(http.MethodPost, "/api/tasks/repo%2Fnormal-task/approve", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
}

func TestAPI_ApproveTask_UnknownTask404(t *testing.T) {
	srv, _, _ := newApprovalTestServer(t, false)
	srv.SetApprovalGate(newFakeGate())

	req := httptest.NewRequest(http.MethodPost, "/api/tasks/ghost/approve", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
}

func TestAPI_ApproveTask_NoGate503(t *testing.T) {
	srv, reg, _ := newApprovalTestServer(t, false)
	registerMinimalTask(t, reg, "repo/x")

	req := httptest.NewRequest(http.MethodPost, "/api/tasks/repo%2Fx/approve", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", w.Code, w.Body.String())
	}
}

func TestAPI_ApproveTask_RequiresAuth(t *testing.T) {
	srv, reg, _ := newApprovalTestServer(t, true)
	registerMinimalTask(t, reg, "repo/pending-task")
	gate := newFakeGate()
	gate.pending["repo/pending-task"] = "hash-1"
	srv.SetApprovalGate(gate)

	// No session, no API key → 401, and the gate must not have been touched.
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/repo%2Fpending-task/approve", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", w.Code, w.Body.String())
	}
	if !gate.IsPending("repo/pending-task") {
		t.Fatal("unauthenticated request must not approve")
	}

	// Garbage Bearer token → still 401.
	req = httptest.NewRequest(http.MethodPost, "/api/tasks/repo%2Fpending-task/approve", nil)
	req.Header.Set("Authorization", "Bearer dck_bogus")
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad key: status = %d, want 401", w.Code)
	}

	// Valid API key → approves.
	raw, _, err := srv.apiKeys.generate(context.Background(), "approve-test")
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/tasks/repo%2Fpending-task/approve", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("api key: status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if gate.IsPending("repo/pending-task") {
		t.Fatal("task still pending after API-key approve")
	}
}

// TestAPI_ApproveTask_RejectsEphemeralToken is the regression for the
// self-approval inversion: an agent's own ephemeral per-run MCP token is a
// valid API key, but it must not let that agent approve the task it just
// authored. The gate must stay pending.
func TestAPI_ApproveTask_RejectsEphemeralToken(t *testing.T) {
	srv, reg, _ := newApprovalTestServer(t, true)
	registerMinimalTask(t, reg, "repo/pending-task")
	gate := newFakeGate()
	gate.pending["repo/pending-task"] = "hash-1"
	srv.SetApprovalGate(gate)

	ephemeral, err := newMCPTokenMinter(srv.apiKeys).Mint(context.Background(), "run-self")
	if err != nil {
		t.Fatalf("mint ephemeral: %v", err)
	}
	// Sanity: it really is a valid key, just not one allowed here.
	if !srv.apiKeys.validate(context.Background(), ephemeral) {
		t.Fatal("precondition: ephemeral token should validate as a key")
	}

	req := httptest.NewRequest(http.MethodPost, "/api/tasks/repo%2Fpending-task/approve", nil)
	req.Header.Set("Authorization", "Bearer "+ephemeral)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("ephemeral token: status = %d, want 401: %s", w.Code, w.Body.String())
	}
	if !gate.IsPending("repo/pending-task") {
		t.Fatal("ephemeral token must not approve the task")
	}
}

// TestAPI_ResumeReplay_RejectEphemeralToken pins that an agent's ephemeral
// per-run token cannot drive resume/replay of arbitrary runs. A 401 at the
// auth layer is enough — the handler is never reached, so the target run need
// not exist.
func TestAPI_ResumeReplay_RejectEphemeralToken(t *testing.T) {
	srv, _, _ := newApprovalTestServer(t, true)
	ephemeral, err := newMCPTokenMinter(srv.apiKeys).Mint(context.Background(), "run-self")
	if err != nil {
		t.Fatalf("mint ephemeral: %v", err)
	}
	h := srv.Handler()

	for _, path := range []string{"/api/runs/other-run/resume", "/api/runs/other-run/replay"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Authorization", "Bearer "+ephemeral)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s with ephemeral token: status = %d, want 401: %s", path, w.Code, w.Body.String())
		}
	}
}

func TestAPI_ApproveTask_SessionCookieWorks(t *testing.T) {
	srv, reg, _ := newApprovalTestServer(t, true)
	registerMinimalTask(t, reg, "repo/pending-task")
	gate := newFakeGate()
	gate.pending["repo/pending-task"] = "hash-1"
	srv.SetApprovalGate(gate)
	h := srv.Handler()

	cookie := login(t, h, "hunter2", false)
	if cookie == nil {
		t.Fatal("login failed")
	}
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/repo%2Fpending-task/approve", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("session: status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if gate.IsPending("repo/pending-task") {
		t.Fatal("task still pending after session approve")
	}
}

// ── Tokenized approve link ───────────────────────────────────────────────────

// tokenFromLink extracts the raw token from a MintApproveLink URL.
func tokenFromLink(t *testing.T, link string) string {
	t.Helper()
	i := strings.LastIndex(link, "/approve/")
	if i < 0 {
		t.Fatalf("link %q has no /approve/ segment", link)
	}
	return link[i+len("/approve/"):]
}

func newTokenLinkServer(t *testing.T) (*Server, *fakeApprovalGate, db.DB) {
	t.Helper()
	srv, reg, d := newApprovalTestServer(t, true) // auth ON: the token must be the only credential needed
	registerMinimalTask(t, reg, "repo/pending-task")
	gate := newFakeGate()
	gate.pending["repo/pending-task"] = "hash-1"
	srv.SetApprovalGate(gate)
	srv.SetApprovalTokenStore(approval.NewTokenStore(d))
	return srv, gate, d
}

func TestApproveLink_MintRequiresPendingTask(t *testing.T) {
	srv, _, _ := newTokenLinkServer(t)
	if _, err := srv.MintApproveLink(context.Background(), "repo/not-pending"); err == nil {
		t.Fatal("minting a link for a non-pending task must fail")
	}
	link, err := srv.MintApproveLink(context.Background(), "repo/pending-task")
	if err != nil {
		t.Fatalf("MintApproveLink: %v", err)
	}
	if !strings.Contains(link, "/approve/dcap_") {
		t.Fatalf("link = %q", link)
	}
}

func TestApproveLink_GetConfirmPageDoesNotConsume(t *testing.T) {
	srv, gate, _ := newTokenLinkServer(t)
	link, err := srv.MintApproveLink(context.Background(), "repo/pending-task")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	token := tokenFromLink(t, link)
	h := srv.Handler()

	// Simulate a prefetcher hammering the GET: nothing may change.
	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/approve/"+token, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET #%d status = %d: %s", i, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "repo/pending-task") {
			t.Fatalf("confirm page must name the task: %s", w.Body.String())
		}
	}
	if !gate.IsPending("repo/pending-task") {
		t.Fatal("GET must not approve")
	}

	// The token is still redeemable afterwards.
	req := httptest.NewRequest(http.MethodPost, "/approve/"+token, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST after GETs: status = %d: %s", w.Code, w.Body.String())
	}
}

func TestApproveLink_RedeemApprovesAndIsSingleUse(t *testing.T) {
	srv, gate, _ := newTokenLinkServer(t)
	link, err := srv.MintApproveLink(context.Background(), "repo/pending-task")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	token := tokenFromLink(t, link)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodPost, "/approve/"+token, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if gate.IsPending("repo/pending-task") {
		t.Fatal("task still pending after redemption")
	}

	// Second redemption: token consumed.
	req = httptest.NewRequest(http.MethodPost, "/approve/"+token, nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("replayed token: status = %d, want 404", w.Code)
	}
}

func TestApproveLink_WrongHashRejected(t *testing.T) {
	srv, gate, _ := newTokenLinkServer(t)
	link, err := srv.MintApproveLink(context.Background(), "repo/pending-task")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	token := tokenFromLink(t, link)

	// The task changes after the token was issued — new observed hash.
	gate.mu.Lock()
	gate.pending["repo/pending-task"] = "hash-2-changed"
	gate.mu.Unlock()

	h := srv.Handler()
	req := httptest.NewRequest(http.MethodPost, "/approve/"+token, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
	if !gate.IsPending("repo/pending-task") {
		t.Fatal("changed task must stay pending — token bound to the old hash")
	}
	// The stale-page GET also refuses.
	req = httptest.NewRequest(http.MethodGet, "/approve/"+token, nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusConflict && w.Code != http.StatusNotFound {
		t.Fatalf("GET with stale binding: status = %d, want 409/404", w.Code)
	}
}

func TestApproveLink_ExpiredRejected(t *testing.T) {
	srv, gate, d := newTokenLinkServer(t)
	link, err := srv.MintApproveLink(context.Background(), "repo/pending-task")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	token := tokenFromLink(t, link)

	// Force-expire the row.
	if err := d.Exec(context.Background(), `UPDATE approval_tokens SET expires_at = 1`); err != nil {
		t.Fatalf("expire: %v", err)
	}

	h := srv.Handler()
	req := httptest.NewRequest(http.MethodPost, "/approve/"+token, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
	if !gate.IsPending("repo/pending-task") {
		t.Fatal("expired token must not approve")
	}
}

func TestApproveLink_BogusTokenRejected(t *testing.T) {
	srv, gate, _ := newTokenLinkServer(t)
	h := srv.Handler()

	for _, tok := range []string{"dcap_bogus", "x", "dcap_" + strings.Repeat("A", 43)} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			req := httptest.NewRequest(method, "/approve/"+tok, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusNotFound {
				t.Errorf("%s /approve/%s: status = %d, want 404", method, tok, w.Code)
			}
		}
	}
	if !gate.IsPending("repo/pending-task") {
		t.Fatal("bogus tokens must not approve")
	}
}
