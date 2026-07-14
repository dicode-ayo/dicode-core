package webui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/source"
	"github.com/dicode/dicode/pkg/task"
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

// TestApiAddSource_DuplicateName verifies that adding a source whose name
// already exists in spec.entries is rejected with 409, and that the
// pre-existing entry is left untouched (config.spec.entries.buildin comes
// from the seed config written by newTestServerWithConfigPath).
func TestApiAddSource_DuplicateName(t *testing.T) {
	srv, _ := newTestServerWithConfigPath(t)

	body := `{"name":"buildin","path":"/tmp/other-tasks"}`
	req := httptest.NewRequest(http.MethodPost, "/api/settings/sources", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("got %d; want 409 for duplicate name; body=%s", w.Code, w.Body.String())
	}

	srv.cfgMu.RLock()
	entry := srv.cfg.Spec.Entries["buildin"]
	srv.cfgMu.RUnlock()
	if entry == nil || entry.Ref == nil {
		t.Fatal("expected pre-existing buildin entry to survive rejected duplicate add")
	}
	if entry.Ref.Path != "/tmp/buildin/taskset.yaml" {
		t.Errorf("buildin entry was overwritten by rejected duplicate add: path = %q", entry.Ref.Path)
	}
}

// TestApiAddSource_ConcurrentDuplicateName_TOCTOU fires two concurrent
// POST /api/settings/sources requests for the same brand-new name and
// asserts exactly one succeeds (200) and the other is rejected (409).
//
// This guards against the check-then-act race in apiAddSource: reading
// cfg.Spec.Entries[name] to decide "does it already exist" and later
// writing cfg.Spec.Entries[name] = entry must happen inside a single
// s.cfgMu.Lock critical section (see the updateConfig mutate callback in
// apiAddSource). If the check and the write were split across two lock
// acquisitions (as they were before this fix, via a separate RLock/RUnlock
// existence check ahead of the later write), two concurrent requests for
// the same never-before-seen name could both observe "not present" and
// both proceed to claim it, so a rerun of this test with that older code
// path intermittently reported 2 OKs (or, depending on write-write
// ordering, silently dropped one caller's config data) instead of exactly
// one OK and one Conflict. Iterating with a fresh name each pass keeps the
// assertion meaningful even though the race window is small.
func TestApiAddSource_ConcurrentDuplicateName_TOCTOU(t *testing.T) {
	srv, _ := newTestServerWithConfigPath(t)

	const iterations = 50
	for i := 0; i < iterations; i++ {
		name := fmt.Sprintf("race-source-%d", i)
		body := `{"name":"` + name + `","path":"/tmp/race"}`

		var wg sync.WaitGroup
		codes := make([]int, 2)
		for g := 0; g < 2; g++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				req := httptest.NewRequest(http.MethodPost, "/api/settings/sources", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				srv.Handler().ServeHTTP(w, req)
				codes[idx] = w.Code
			}(g)
		}
		wg.Wait()

		okCount, conflictCount := 0, 0
		for _, c := range codes {
			switch c {
			case http.StatusOK:
				okCount++
			case http.StatusConflict:
				conflictCount++
			default:
				t.Fatalf("iteration %d: unexpected status %d for %q", i, c, name)
			}
		}
		if okCount != 1 || conflictCount != 1 {
			t.Fatalf("iteration %d: got %d OK / %d Conflict for two concurrent adds of %q; want exactly 1 OK and 1 Conflict (TOCTOU race in the duplicate-name check)", i, okCount, conflictCount, name)
		}

		srv.cfgMu.RLock()
		_, exists := srv.cfg.Spec.Entries[name]
		srv.cfgMu.RUnlock()
		if !exists {
			t.Fatalf("iteration %d: winning request's entry %q missing from cfg.Spec.Entries after both requests completed", i, name)
		}
	}
}

// TestApiAddSource_InvalidNameSlug verifies that a name containing
// characters unsafe for a spec.entries key / task-id namespace segment
// (here, a path separator) is rejected with 400 and never persisted.
func TestApiAddSource_InvalidNameSlug(t *testing.T) {
	srv, _ := newTestServerWithConfigPath(t)

	body := `{"name":"bad/name","path":"/tmp/tasks"}`
	req := httptest.NewRequest(http.MethodPost, "/api/settings/sources", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d; want 400 for invalid name slug; body=%s", w.Code, w.Body.String())
	}

	srv.cfgMu.RLock()
	_, exists := srv.cfg.Spec.Entries["bad/name"]
	srv.cfgMu.RUnlock()
	if exists {
		t.Error("invalid name should not have been persisted to spec.entries")
	}
}

// TestApiAddSource_ValidSlugName is the positive counterpart to
// TestApiAddSource_InvalidNameSlug: a name using the full allowed character
// set (letters, digits, '-', '_', '.') is accepted.
func TestApiAddSource_ValidSlugName(t *testing.T) {
	srv, _ := newTestServerWithConfigPath(t)

	body := `{"name":"my-source_2.local","path":"/tmp/tasks"}`
	req := httptest.NewRequest(http.MethodPost, "/api/settings/sources", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d; want 200 for valid slug name; body=%s", w.Code, w.Body.String())
	}

	srv.cfgMu.RLock()
	entry := srv.cfg.Spec.Entries["my-source_2.local"]
	srv.cfgMu.RUnlock()
	if entry == nil || entry.Ref == nil {
		t.Fatal("expected my-source_2.local entry in config after add")
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

// TestApiSaveConfigRaw_ReResolvesSourceOverrides asserts parity between the two
// override-mutation surfaces: a raw-config save that changes an entry's
// overrides must drive the same re-resolve → EventUpdated → re-Admit pipeline
// that PATCH /api/tasks/{id}/overrides does. Before #408 the editor path only
// refreshed the REST snapshot via SetCfg and left the running taskset.Source's
// parentOverrides stale until restart — a revoked permission elevation kept
// running with the broader grant. The Source emitting EventUpdated with the
// new (disabled) resolved spec proves SetParentOverrides was applied.
func TestApiSaveConfigRaw_ReResolvesSourceOverrides(t *testing.T) {
	repoDir := t.TempDir()
	taskDir := filepath.Join(repoDir, "deploy")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatalf("mkdir task: %v", err)
	}
	taskYAML := "kind: Task\napiVersion: dicode/v1\nname: deploy\nruntime: deno\ntrigger:\n  cron: \"0 8 * * *\"\n"
	if err := os.WriteFile(filepath.Join(taskDir, "task.yaml"), []byte(taskYAML), 0o644); err != nil {
		t.Fatalf("write task.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "task.js"), []byte("// task"), 0o644); err != nil {
		t.Fatalf("write task.js: %v", err)
	}
	tsPath := filepath.Join(repoDir, "taskset.yaml")
	tsContent := "apiVersion: dicode/v1\nkind: TaskSet\nmetadata:\n  name: buildin\nspec:\n  entries:\n    deploy:\n      ref:\n        path: " + filepath.Join(taskDir, "task.yaml") + "\n"
	if err := os.WriteFile(tsPath, []byte(tsContent), 0o644); err != nil {
		t.Fatalf("write taskset.yaml: %v", err)
	}

	src := taskset.NewSource(
		"buildin", "buildin",
		&taskset.Ref{Path: tsPath},
		"", t.TempDir(), false, time.Hour, // long poll so only the refresh signal fires
		zap.NewNop(),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ch, err := src.Start(ctx)
	if err != nil {
		t.Fatalf("Start source: %v", err)
	}

	// Drain the initial Added event; the task starts enabled.
	select {
	case ev := <-ch:
		if ev.Kind != source.EventAdded {
			t.Fatalf("first event kind = %v, want Added", ev.Kind)
		}
		if !ev.Kinded.(*task.Spec).Enabled {
			t.Fatal("initial spec should be Enabled=true")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for initial Added event")
	}

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")
	initialYAML := "spec:\n  entries:\n    buildin:\n      ref:\n        path: " + tsPath + "\n"
	if err := os.WriteFile(cfgPath, []byte(initialYAML), 0o600); err != nil {
		t.Fatalf("write initial cfg: %v", err)
	}
	initialCfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load initial cfg: %v", err)
	}
	sourceMgr := NewSourceManager(initialCfg, map[string]*taskset.Source{"buildin": src}, dir, zap.NewNop())
	srv, _ := newTestServerWithSourceMgr(t, initialCfg, cfgPath, sourceMgr)

	// Raw-config save that revokes the task via an entry override.
	updatedYAML := "spec:\n  entries:\n    buildin:\n      ref:\n        path: " + tsPath + "\n      overrides:\n        entries:\n          deploy:\n            enabled: false\n"
	body := `{"content":` + string(mustJSON(t, updatedYAML)) + `}`
	req := httptest.NewRequest(http.MethodPost, "/api/config/raw", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/config/raw returned %d; body=%s", w.Code, w.Body.String())
	}

	// The running source must re-resolve and emit Updated with the now-disabled
	// spec — identical to the PATCH path's outcome.
	select {
	case ev := <-ch:
		if ev.Kind != source.EventUpdated {
			t.Fatalf("post-save event kind = %v, want Updated", ev.Kind)
		}
		if ev.Kinded.(*task.Spec).Enabled {
			t.Error("post-save resolved spec.Enabled = true, want false (override not applied to running source)")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for refresh-driven Updated event after raw-config save")
	}
}
