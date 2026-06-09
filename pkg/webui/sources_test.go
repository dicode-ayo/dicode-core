package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/taskset"
	"go.uber.org/zap"
)

// TestSourceManager_List_LocalSource_NoPullFieldsInJSON guards the
// wire format for the frontend: a local source must serialize WITHOUT
// a last_pull_at field, so the client's `if (!src.last_pull_at)` check
// succeeds and no dot is rendered.
//
// This is the regression the pr-review-toolkit flagged: `time.Time` +
// `omitempty` emits `"0001-01-01T00:00:00Z"`, which is truthy in JS.
// Using a *time.Time pointer fixes it.
func TestSourceManager_List_LocalSource_NoPullFieldsInJSON(t *testing.T) {
	watchTrue := true
	cfg := &config.Config{}
	cfg.Spec.Entries = map[string]*taskset.Entry{
		"tasks": {Ref: &taskset.Ref{Path: "/tmp/tasks", Watch: &watchTrue}},
	}
	m := NewSourceManager(cfg, nil, t.TempDir(), zap.NewNop())

	b, err := json.Marshal(m.List())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "last_pull_at") {
		t.Errorf("local source JSON should omit last_pull_at; got %s", b)
	}
	if strings.Contains(string(b), "last_pull_error") {
		t.Errorf("local source JSON should omit last_pull_error; got %s", b)
	}
}

// TestApiSetDevMode_DecodesBranchBody verifies that the new branch/base/run_id
// JSON fields are wired through the handler's decode path without error.
// With a nil SourceManager (the default newTestServer setup), the handler
// returns 503 "source manager not configured" AFTER successfully parsing the
// body. A 400 would mean the JSON parse failed — that's what we guard against.
func TestApiSetDevMode_DecodesBranchBody(t *testing.T) {
	srv, _ := newTestServer(t)

	body := `{"enabled":true,"branch":"fix/test","base":"main","run_id":"r1"}`
	req := httptest.NewRequest(http.MethodPatch, "/api/sources/fixture/dev", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code == http.StatusBadRequest {
		t.Fatalf("got 400 BadRequest; body parse failed for new fields. body=%s", w.Body.String())
	}
}

// TestApiSetDevMode_RejectsMalformedJson verifies that malformed JSON in the
// request body is rejected with 400 BadRequest before any SourceManager check.
func TestApiSetDevMode_RejectsMalformedJson(t *testing.T) {
	srv, _ := newTestServer(t)

	body := `not-json`
	req := httptest.NewRequest(http.MethodPatch, "/api/sources/fixture/dev", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("got %d; want 400 BadRequest for malformed body. body=%s", w.Code, w.Body.String())
	}
}

// TestApiSaveConfigRaw_SyncsSourceManagerCfg verifies that after a raw config
// save via POST /api/config/raw, SourceManager.cfg is updated to the new
// *config.Config pointer so that subsequent List() calls reflect the new sources.
// This is the regression guard for issue #268.
func TestApiSaveConfigRaw_SyncsSourceManagerCfg(t *testing.T) {
	// Write initial dicode.yaml with one source entry.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")
	initialYAML := "server:\n  port: 8080\nspec:\n  entries:\n    old-source:\n      ref:\n        path: /tmp/old\n"
	if err := os.WriteFile(cfgPath, []byte(initialYAML), 0600); err != nil {
		t.Fatalf("write initial config: %v", err)
	}

	// Build initial config and SourceManager from it.
	initialCfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load initial config: %v", err)
	}
	sourceMgr := NewSourceManager(initialCfg, nil, dir, zap.NewNop())

	// Build server with the SourceManager and the cfgPath so the handler can
	// write and hot-reload the config.
	srv, _ := newTestServerWithSourceMgr(t, initialCfg, cfgPath, sourceMgr)

	// POST updated config that removes old-source and adds new-source.
	updatedYAML := "server:\n  port: 8080\nspec:\n  entries:\n    new-source:\n      ref:\n        path: /tmp/new\n"
	body := `{"content":` + string(mustJSON(t, updatedYAML)) + `}`
	req := httptest.NewRequest(http.MethodPost, "/api/config/raw", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/config/raw returned %d; body=%s", w.Code, w.Body.String())
	}

	// SourceManager.List() must now show new-source, not old-source.
	sources := sourceMgr.List()
	found := false
	for _, s := range sources {
		if s.Name == "new-source" {
			found = true
		}
		if s.Name == "old-source" {
			t.Errorf("SourceManager still lists old-source after raw config save; cfg was not synced")
		}
	}
	if !found {
		t.Errorf("SourceManager does not list new-source after raw config save; got %+v", sources)
	}
}

// mustJSON marshals v as JSON, failing the test on error.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

// ---------------------------------------------------------------------------
// HTTP-level tests for apiAddSource / apiRemoveSource (#263)
// ---------------------------------------------------------------------------

// TestApiAddSource_LocalPath adds a local-path source via POST and verifies
// it appears in the server config.
func TestApiAddSource_LocalPath(t *testing.T) {
	srv, _ := newTestServerWithConfigPath(t)

	body := `{"name":"my-local","path":"/tmp/tasks"}`
	req := httptest.NewRequest(http.MethodPost, "/api/settings/sources", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/settings/sources returned %d; body=%s", w.Code, w.Body.String())
	}

	srv.cfgMu.RLock()
	entry := srv.cfg.Spec.Entries["my-local"]
	srv.cfgMu.RUnlock()
	if entry == nil || entry.Ref == nil {
		t.Fatal("expected my-local entry in config after add")
	}
	if entry.Ref.Path != "/tmp/tasks" {
		t.Errorf("path = %q, want /tmp/tasks", entry.Ref.Path)
	}
}

// TestApiAddSource_GitURL adds a git source and verifies branch + URL appear.
func TestApiAddSource_GitURL(t *testing.T) {
	srv, _ := newTestServerWithConfigPath(t)

	body := `{"name":"my-git","url":"https://github.com/example/repo.git","branch":"develop"}`
	req := httptest.NewRequest(http.MethodPost, "/api/settings/sources", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/settings/sources returned %d; body=%s", w.Code, w.Body.String())
	}

	srv.cfgMu.RLock()
	entry := srv.cfg.Spec.Entries["my-git"]
	srv.cfgMu.RUnlock()
	if entry == nil || entry.Ref == nil {
		t.Fatal("expected my-git entry in config after add")
	}
	if entry.Ref.URL != "https://github.com/example/repo.git" {
		t.Errorf("url = %q, want https://github.com/example/repo.git", entry.Ref.URL)
	}
	if entry.Ref.Branch != "develop" {
		t.Errorf("branch = %q, want develop", entry.Ref.Branch)
	}
}

// TestApiAddSource_MissingName verifies that a missing name returns 400.
func TestApiAddSource_MissingName(t *testing.T) {
	srv, _ := newTestServerWithConfigPath(t)

	body := `{"path":"/tmp/tasks"}`
	req := httptest.NewRequest(http.MethodPost, "/api/settings/sources", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("got %d; want 400 for missing name", w.Code)
	}
}

// TestApiAddSource_MissingURLAndPath verifies that omitting both url and path
// returns 400.
func TestApiAddSource_MissingURLAndPath(t *testing.T) {
	srv, _ := newTestServerWithConfigPath(t)

	body := `{"name":"empty"}`
	req := httptest.NewRequest(http.MethodPost, "/api/settings/sources", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("got %d; want 400 for missing url and path", w.Code)
	}
}

// TestApiRemoveSource_HappyPath adds then removes a source, verifying it
// disappears from the config.
func TestApiRemoveSource_HappyPath(t *testing.T) {
	srv, _ := newTestServerWithConfigPath(t)

	// Add a source first.
	addBody := `{"name":"ephemeral","path":"/tmp/eph"}`
	addReq := httptest.NewRequest(http.MethodPost, "/api/settings/sources", strings.NewReader(addBody))
	addReq.Header.Set("Content-Type", "application/json")
	addW := httptest.NewRecorder()
	srv.Handler().ServeHTTP(addW, addReq)
	if addW.Code != http.StatusOK {
		t.Fatalf("add returned %d; body=%s", addW.Code, addW.Body.String())
	}

	// Remove it.
	delReq := httptest.NewRequest(http.MethodDelete, "/api/settings/sources/ephemeral", nil)
	delW := httptest.NewRecorder()
	srv.Handler().ServeHTTP(delW, delReq)
	if delW.Code != http.StatusOK {
		t.Fatalf("delete returned %d; body=%s", delW.Code, delW.Body.String())
	}

	srv.cfgMu.RLock()
	entry := srv.cfg.Spec.Entries["ephemeral"]
	srv.cfgMu.RUnlock()
	if entry != nil {
		t.Error("expected ephemeral to be removed from config, but it still exists")
	}
}

// TestApiRemoveSource_NotFound verifies that removing a non-existent source
// returns 404.
func TestApiRemoveSource_NotFound(t *testing.T) {
	srv, _ := newTestServerWithConfigPath(t)

	req := httptest.NewRequest(http.MethodDelete, "/api/settings/sources/does-not-exist", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("got %d; want 404 for non-existent source", w.Code)
	}
}
