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
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/trigger"
	"go.uber.org/zap"
)

// fakeApprovalGate implements ApprovalGate over an in-memory pending map.
type fakeApprovalGate struct {
	mu       sync.Mutex
	pending  map[string]string // task id → observed hash
	approved []string
	diffs    map[string]approval.Diff // task id → canned Diff for the test to assert against
	diffErrs map[string]error         // task id → error Diff should return instead
}

func newFakeGate() *fakeApprovalGate {
	return &fakeApprovalGate{pending: map[string]string{}}
}

// setDiff stashes the Diff Diff(id) should return, for tests that don't need
// the real gate's snapshot machinery.
func (g *fakeApprovalGate) setDiff(id string, d approval.Diff) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.diffs == nil {
		g.diffs = map[string]approval.Diff{}
	}
	g.diffs[id] = d
}

func (g *fakeApprovalGate) Diff(id string) (approval.Diff, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err, ok := g.diffErrs[id]; ok {
		return approval.Diff{}, err
	}
	if d, ok := g.diffs[id]; ok {
		return d, nil
	}
	if _, ok := g.pending[id]; !ok {
		return approval.Diff{}, fmt.Errorf("task %q is not pending approval", id)
	}
	return approval.Diff{TaskID: id}, nil
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
		// Wrap the same sentinel the real gate wraps (pkg/approval/gate.go's
		// approve()), since apiApproveTask distinguishes this case via
		// errors.Is — a fake that returned a merely similar-looking error
		// would let that branch silently go untested.
		return fmt.Errorf("task %q changed since the approval was issued: %w", id, approval.ErrHashMismatch)
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

// TestAPI_ApproveTask_HashBindsToPendingVersion locks in #645: a request
// carrying the hash the operator's diff was built from routes through
// ApproveIfHash and succeeds when that hash still matches what's pending.
func TestAPI_ApproveTask_HashBindsToPendingVersion(t *testing.T) {
	srv, reg, _ := newApprovalTestServer(t, false)
	registerMinimalTask(t, reg, "repo/pending-task")
	gate := newFakeGate()
	gate.pending["repo/pending-task"] = "hash-reviewed"
	srv.SetApprovalGate(gate)

	req := httptest.NewRequest(http.MethodPost, "/api/tasks/repo%2Fpending-task/approve",
		strings.NewReader(`{"hash":"hash-reviewed"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if gate.IsPending("repo/pending-task") {
		t.Error("task still pending after hash-bound approve")
	}
}

// TestAPI_ApproveTask_StaleHash409 is the regression this issue exists for:
// before binding approval to the reviewed hash, apiApproveTask always called
// the unconditional Approve and would have armed whatever is CURRENTLY
// pending regardless of the hash sent — silently approving a version the
// operator never saw. Posting a hash that no longer matches what's pending
// must now be rejected, with the task left pending, and the response must
// say "stale" so the dashboard knows to refetch rather than just failing.
func TestAPI_ApproveTask_StaleHash409(t *testing.T) {
	srv, reg, _ := newApprovalTestServer(t, false)
	registerMinimalTask(t, reg, "repo/pending-task")
	gate := newFakeGate()
	// The task re-pended at a newer hash after the operator's diff loaded.
	gate.pending["repo/pending-task"] = "hash-newer"
	srv.SetApprovalGate(gate)

	req := httptest.NewRequest(http.MethodPost, "/api/tasks/repo%2Fpending-task/approve",
		strings.NewReader(`{"hash":"hash-stale-review"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if stale, _ := body["stale"].(bool); !stale {
		t.Errorf(`body["stale"] = %v, want true: %s`, body["stale"], w.Body.String())
	}
	if !gate.IsPending("repo/pending-task") {
		t.Error("task must stay pending after a rejected stale-hash approval")
	}
	if len(gate.approved) != 0 {
		t.Errorf("approve must not have run: approved = %v", gate.approved)
	}
}

// TestAPI_ApproveTask_MalformedBody400 ensures a caller that does send a
// body gets a clear 400 on genuinely invalid JSON, distinct from the
// no-body-at-all case (which must still hit the unbound Approve path — see
// TestAPI_ApproveTask_ApprovesPending).
func TestAPI_ApproveTask_MalformedBody400(t *testing.T) {
	srv, reg, _ := newApprovalTestServer(t, false)
	registerMinimalTask(t, reg, "repo/pending-task")
	gate := newFakeGate()
	gate.pending["repo/pending-task"] = "hash-1"
	srv.SetApprovalGate(gate)

	req := httptest.NewRequest(http.MethodPost, "/api/tasks/repo%2Fpending-task/approve",
		strings.NewReader(`{not json`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if !gate.IsPending("repo/pending-task") {
		t.Error("task must stay pending when the request body is malformed")
	}
}

// TestAPI_ApproveTask_ExplicitEmptyHash400 guards against a request that
// meant to bind but sent a zero-value hash (e.g. a dashboard bug reading a
// falsy pending_hash) silently degrading into the unconditional-approve
// path with no signal. An absent "hash" field still means "unbound" (see
// TestAPI_ApproveTask_ApprovesPending); an explicitly empty one is rejected.
func TestAPI_ApproveTask_ExplicitEmptyHash400(t *testing.T) {
	srv, reg, _ := newApprovalTestServer(t, false)
	registerMinimalTask(t, reg, "repo/pending-task")
	gate := newFakeGate()
	gate.pending["repo/pending-task"] = "hash-1"
	srv.SetApprovalGate(gate)

	req := httptest.NewRequest(http.MethodPost, "/api/tasks/repo%2Fpending-task/approve",
		strings.NewReader(`{"hash":""}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if !gate.IsPending("repo/pending-task") {
		t.Error("task must stay pending when the hash field is explicitly empty")
	}
	if len(gate.approved) != 0 {
		t.Errorf("approve must not have run: approved = %v", gate.approved)
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

	ephemeral, err := newMCPTokenMinter(srv.apiKeys).Mint(context.Background(), "run-self", pkgruntime.MCPScope{ListTasks: true, RunTaskIDs: []string{"*"}})
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
	ephemeral, err := newMCPTokenMinter(srv.apiKeys).Mint(context.Background(), "run-self", pkgruntime.MCPScope{ListTasks: true, RunTaskIDs: []string{"*"}})
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

// ── GET /api/tasks/{id}/pending-diff ─────────────────────────────────────────

func TestAPI_ApprovalDiff_HappyPath(t *testing.T) {
	srv, reg, _ := newApprovalTestServer(t, false)
	registerMinimalTask(t, reg, "repo/pending-task")
	gate := newFakeGate()
	gate.pending["repo/pending-task"] = "hash-1"
	gate.setDiff("repo/pending-task", approval.Diff{
		TaskID:      "repo/pending-task",
		HasBaseline: true,
		Files: []approval.FileDiff{
			{Path: "task.js", Status: "modified", UnifiedDiff: "- old\n+ new\n", SecurityRelevant: false},
		},
	})
	srv.SetApprovalGate(gate)

	req := httptest.NewRequest(http.MethodGet, "/api/tasks/repo%2Fpending-task/pending-diff", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var d approval.Diff
	if err := json.NewDecoder(w.Body).Decode(&d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !d.HasBaseline {
		t.Error("HasBaseline = false, want true")
	}
	if len(d.Files) != 1 || d.Files[0].Path != "task.js" {
		t.Fatalf("Files = %+v", d.Files)
	}
}

func TestAPI_ApprovalDiff_UnknownTask404(t *testing.T) {
	srv, _, _ := newApprovalTestServer(t, false)
	srv.SetApprovalGate(newFakeGate())

	req := httptest.NewRequest(http.MethodGet, "/api/tasks/ghost/pending-diff", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
	}
}

func TestAPI_ApprovalDiff_NotPending409(t *testing.T) {
	srv, reg, _ := newApprovalTestServer(t, false)
	registerMinimalTask(t, reg, "repo/normal-task")
	srv.SetApprovalGate(newFakeGate())

	req := httptest.NewRequest(http.MethodGet, "/api/tasks/repo%2Fnormal-task/pending-diff", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
}

func TestAPI_ApprovalDiff_NoGate503(t *testing.T) {
	srv, reg, _ := newApprovalTestServer(t, false)
	registerMinimalTask(t, reg, "repo/x")

	req := httptest.NewRequest(http.MethodGet, "/api/tasks/repo%2Fx/pending-diff", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", w.Code, w.Body.String())
	}
}

func TestAPI_ApprovalDiff_RequiresAuth(t *testing.T) {
	srv, reg, _ := newApprovalTestServer(t, true)
	registerMinimalTask(t, reg, "repo/pending-task")
	gate := newFakeGate()
	gate.pending["repo/pending-task"] = "hash-1"
	srv.SetApprovalGate(gate)

	req := httptest.NewRequest(http.MethodGet, "/api/tasks/repo%2Fpending-task/pending-diff", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", w.Code, w.Body.String())
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

// TestApproveLink_ConfirmPageShowsDiff is the regression for #604 on the
// tokenized link surface: the confirm page must render the pending diff
// (the changed file, its +/- lines, and a security-relevant warning) so an
// operator approving from a notification link — no session, no dashboard —
// isn't approving blind either.
func TestApproveLink_ConfirmPageShowsDiff(t *testing.T) {
	srv, gate, _ := newTokenLinkServer(t)
	gate.setDiff("repo/pending-task", approval.Diff{
		TaskID:      "repo/pending-task",
		HasBaseline: true,
		Files: []approval.FileDiff{
			{
				Path:             "task.yaml",
				Status:           "modified",
				UnifiedDiff:      "  name: repo/pending-task\n- permissions: {}\n+ permissions:\n+   net: [\"*\"]\n",
				SecurityRelevant: true,
			},
		},
	})
	link, err := srv.MintApproveLink(context.Background(), "repo/pending-task")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	token := tokenFromLink(t, link)

	req := httptest.NewRequest(http.MethodGet, "/approve/"+token, nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "task.yaml") {
		t.Errorf("confirm page missing changed file name: %s", body)
	}
	if !strings.Contains(body, "net") {
		t.Errorf("confirm page missing diff content: %s", body)
	}
	if !strings.Contains(body, "security-relevant") {
		t.Errorf("confirm page missing security-relevant badge: %s", body)
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
