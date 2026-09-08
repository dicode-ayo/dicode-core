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
	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/source"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/taskset"
	"github.com/dicode/dicode/pkg/trigger"
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
	m := NewSourceManager(cfg, nil, nil, t.TempDir(), zap.NewNop())

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

// TestSourceManager_List_TasksetSource_FailedCount is the #649 regression
// lock for the Sources page: a taskset source with one entry that fails to
// parse must report a non-zero failed_count (and the per-entry detail) via
// GET /api/sources, so the source-health dot can stop claiming "all clear".
// A sibling entry that resolves fine must not be affected.
func TestSourceManager_List_TasksetSource_FailedCount(t *testing.T) {
	dir := t.TempDir()

	writeMiniTask := func(name, extraYAML string) string {
		td := filepath.Join(dir, name)
		if err := os.MkdirAll(td, 0755); err != nil {
			t.Fatal(err)
		}
		yaml := "kind: Task\napiVersion: dicode/v1\nname: " + name + "\nruntime: deno\ntrigger:\n  manual: true\n" + extraYAML
		if err := os.WriteFile(filepath.Join(td, "task.yaml"), []byte(yaml), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(td, "task.js"), []byte("// task"), 0644); err != nil {
			t.Fatal(err)
		}
		return filepath.Join(td, "task.yaml")
	}
	goodPath := writeMiniTask("good", "")
	// hash_include is a []string field; a bool value fails to unmarshal —
	// the same class of typo #649 quotes from daemon.log.
	badPath := writeMiniTask("bad", "hash_include: true\n")

	tsContent := "apiVersion: dicode/v1\nkind: TaskSet\nmetadata:\n  name: infra\nspec:\n  entries:\n" +
		"    good:\n      ref:\n        path: " + goodPath + "\n" +
		"    bad:\n      ref:\n        path: " + badPath + "\n"
	tsPath := filepath.Join(dir, "taskset.yaml")
	if err := os.WriteFile(tsPath, []byte(tsContent), 0644); err != nil {
		t.Fatal(err)
	}

	src := taskset.NewSource("src-id", "infra", &taskset.Ref{Path: tsPath}, "", t.TempDir(), false, 30*time.Second, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := src.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cfg := &config.Config{}
	cfg.Spec.Entries = map[string]*taskset.Entry{
		"infra": {Ref: &taskset.Ref{Path: tsPath}},
	}
	m := NewSourceManager(cfg, map[string]*taskset.Source{"infra": src}, nil, t.TempDir(), zap.NewNop())

	infos := m.List()
	if len(infos) != 1 {
		t.Fatalf("want 1 source, got %d: %+v", len(infos), infos)
	}
	info := infos[0]
	if info.FailedCount != 1 {
		t.Fatalf("FailedCount = %d, want 1 (only the bad entry): %+v", info.FailedCount, info)
	}
	if len(info.Failures) != 1 || info.Failures[0].ID != "infra/bad" {
		t.Errorf("Failures = %+v, want exactly infra/bad", info.Failures)
	}

	// SourceManager.LoadFailures aggregates the same data across sources for
	// the task-list merge (apiListTasks).
	agg := m.LoadFailures()
	if _, ok := agg["infra/bad"]; !ok {
		t.Errorf("LoadFailures() = %v, want infra/bad present", agg)
	}
	if _, ok := agg["infra/good"]; ok {
		t.Errorf("LoadFailures() should not include the good entry: %v", agg)
	}
}

// TestSourceManager_List_NonTasksetSource_FailedCount is the regression lock
// for the gap a PR #656 review pass found: List()'s `else if ref.IsGit()` /
// `else` branches — taken for a source name that's in cfg.Spec.Entries but
// has no live *taskset.Source yet (a plain git/local source in steady state,
// or one still mid-registration via apiAddSource's claim-then-Register
// window) — never consulted any load-failure state at all, so those sources
// kept reporting FailedCount == 0 even when the registry had a recorded
// failure for one of their tasks (reconciler.go's handle() calls
// registry.SetLoadFailure for exactly this event shape: ev.Kinded == nil,
// i.e. not pre-resolved by a taskset.Source). Covers both "git" and "local"
// typed entries, and confirms a failure recorded against one source name
// never leaks into another's count (SourceManager has no live taskset.Source
// to consult for either, so both entries below use the tasksetSources==nil
// path exactly like the previously-uncovered branches).
func TestSourceManager_List_NonTasksetSource_FailedCount(t *testing.T) {
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)

	cfg := &config.Config{}
	cfg.Spec.Entries = map[string]*taskset.Entry{
		"local-src": {Ref: &taskset.Ref{Path: "/tmp/local-tasks"}},
		"git-src":   {Ref: &taskset.Ref{URL: "https://example.com/repo.git", Branch: "main"}},
	}
	m := NewSourceManager(cfg, nil, reg, t.TempDir(), zap.NewNop())

	reg.SetLoadFailure("local-src/bad", "local-src", "yaml: line 3: hash_include must be a list")
	reg.SetLoadFailure("git-src/broken", "git-src", "yaml: unknown field foo")

	infos := m.List()
	byName := make(map[string]SourceInfo, len(infos))
	for _, i := range infos {
		byName[i.Name] = i
	}

	local, ok := byName["local-src"]
	if !ok {
		t.Fatalf("local-src missing from List(): %+v", infos)
	}
	if local.Type != "local" {
		t.Fatalf("local-src Type = %q, want local", local.Type)
	}
	if local.FailedCount != 1 || len(local.Failures) != 1 || local.Failures[0].ID != "local-src/bad" {
		t.Errorf("local-src failures = %+v, want exactly local-src/bad", local)
	}

	git, ok := byName["git-src"]
	if !ok {
		t.Fatalf("git-src missing from List(): %+v", infos)
	}
	if git.Type != "git" {
		t.Fatalf("git-src Type = %q, want git", git.Type)
	}
	if git.FailedCount != 1 || len(git.Failures) != 1 || git.Failures[0].ID != "git-src/broken" {
		t.Errorf("git-src failures = %+v, want exactly git-src/broken", git)
	}
}

// TestSourceManager_List_NonTasksetSource_NilRegistry guards against a nil
// panic: SourceManager.registry is nil in many test setups (and — briefly —
// would be nil in production if this constructor arg were ever omitted), so
// List() must tolerate it rather than dereferencing a nil *registry.Registry.
func TestSourceManager_List_NonTasksetSource_NilRegistry(t *testing.T) {
	cfg := &config.Config{}
	cfg.Spec.Entries = map[string]*taskset.Entry{
		"local-src": {Ref: &taskset.Ref{Path: "/tmp/local-tasks"}},
	}
	m := NewSourceManager(cfg, nil, nil, t.TempDir(), zap.NewNop())

	infos := m.List()
	if len(infos) != 1 || infos[0].FailedCount != 0 {
		t.Fatalf("List() = %+v, want one failure-free local-src", infos)
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
	sourceMgr := NewSourceManager(initialCfg, nil, nil, dir, zap.NewNop())

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

// TestApiSaveConfigRaw_RejectsSemanticallyInvalidConfig_NoWrite is the
// regression guard for #806: apiSaveConfigRaw checked only that the
// submitted content parsed as a YAML mapping, then wrote it to dicode.yaml
// before calling config.Load to hot-reload. When that reload failed
// validation, the error was logged and swallowed — the response was still
// 200 OK, the invalid config was already on disk, and the daemon would
// refuse to start on the next restart with no link back to this edit.
//
// server.public_url without server.auth: true is syntactically valid YAML
// that has always failed cfg.validate() — exactly the class of error the
// raw-mapping check let through. The endpoint must now reject it with 400
// and, critically, must never have written it to disk.
func TestApiSaveConfigRaw_RejectsSemanticallyInvalidConfig_NoWrite(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")
	originalYAML := "server:\n  port: 8080\nspec:\n  entries:\n    old-source:\n      ref:\n        path: /tmp/old\n"
	if err := os.WriteFile(cfgPath, []byte(originalYAML), 0600); err != nil {
		t.Fatalf("write initial config: %v", err)
	}

	initialCfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load initial config: %v", err)
	}
	srv, _ := newTestServerWithSourceMgr(t, initialCfg, cfgPath, nil)

	invalidYAML := "server:\n  port: 8080\n  public_url: https://dicode.example.com\nspec:\n  entries:\n    old-source:\n      ref:\n        path: /tmp/old\n"
	body := `{"content":` + string(mustJSON(t, invalidYAML)) + `}`
	req := httptest.NewRequest(http.MethodPost, "/api/config/raw", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/config/raw returned %d, want 400; body=%s", w.Code, w.Body.String())
	}

	onDisk, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read cfgPath: %v", err)
	}
	if string(onDisk) != originalYAML {
		t.Errorf("dicode.yaml was modified despite a 400 response:\ngot:\n%s\nwant (unchanged):\n%s", onDisk, originalYAML)
	}

	// The in-memory config must also be untouched.
	srv.cfgMu.RLock()
	gotPort := srv.cfg.Server.Port
	srv.cfgMu.RUnlock()
	if gotPort != initialCfg.Server.Port {
		t.Errorf("srv.cfg was mutated despite a 400 response: port = %d, want %d", gotPort, initialCfg.Server.Port)
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

// TestApiAddSource_SharedPathWithExistingSource_RemoveDoesNotOrphanNames is
// HTTP-level coverage for issue #621's scenario: adding a second source
// through the real API whose path is identical to an already-configured
// source's path (e.g. add-source.spec.ts's first test pointing at the same
// taskset.yaml an "e2e-tests" source already watches). It exercises the real
// apiAddSource/apiRemoveSource handlers end-to-end (via
// newTestServerWithReconciler) and confirms cfg.Spec.Entries bookkeeping for
// the two names stays independent.
//
// Note this alone would NOT have caught the Source.ID() collision fixed by
// taskset.SourceID: cfg.Spec.Entries is keyed by name, not by Source.ID(), so
// this test's assertions pass identically with or without that fix. The
// actual regression proof — that rc.cancels itself keeps the two sources'
// teardown bookkeeping independent, which DOES fail without the fix — is
// pkg/registry/reconciler_test.go's
// TestReconciler_NameQualifiedSourceIDs_RemoveDoesNotClobberOther. This test
// is kept as HTTP-level sanity coverage for the same scenario.
func TestApiAddSource_SharedPathWithExistingSource_RemoveDoesNotOrphanNames(t *testing.T) {
	repoDir := t.TempDir()
	taskDir := filepath.Join(repoDir, "hello")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatalf("mkdir task: %v", err)
	}
	taskYAML := "kind: Task\napiVersion: dicode/v1\nname: hello\nruntime: deno\ntrigger:\n  manual: true\n"
	if err := os.WriteFile(filepath.Join(taskDir, "task.yaml"), []byte(taskYAML), 0o644); err != nil {
		t.Fatalf("write task.yaml: %v", err)
	}
	tsPath := filepath.Join(repoDir, "taskset.yaml")
	tsContent := "apiVersion: dicode/v1\nkind: TaskSet\nmetadata:\n  name: e2e-tests\nspec:\n  entries:\n    hello:\n      ref:\n        path: " + filepath.Join(taskDir, "task.yaml") + "\n"
	if err := os.WriteFile(tsPath, []byte(tsContent), 0o644); err != nil {
		t.Fatalf("write taskset.yaml: %v", err)
	}

	// "e2e-tests" is pre-configured (mirrors the fixture-seeded source),
	// already watching tsPath before the dynamic add.
	watchTrue := true
	cfg := &config.Config{}
	cfg.Spec.Entries = map[string]*taskset.Entry{
		"e2e-tests": {Ref: &taskset.Ref{Path: tsPath, Watch: &watchTrue}},
	}
	srv, rec, _ := newTestServerWithReconciler(t, cfg)

	preexisting := taskset.NewSource(
		taskset.SourceID("e2e-tests", cfg.Spec.Entries["e2e-tests"].Ref),
		"e2e-tests", cfg.Spec.Entries["e2e-tests"].Ref, "", t.TempDir(), false, time.Hour, zap.NewNop(),
	)
	if err := rec.AddSource(preexisting); err != nil {
		t.Fatalf("seed pre-existing source: %v", err)
	}

	// Add a second source, through the real HTTP handler, pointed at the
	// IDENTICAL taskset.yaml — exactly add-source.spec.ts's first test.
	addName := "e2e-add-local-621"
	addBody := `{"name":"` + addName + `","path":"` + strings.ReplaceAll(tsPath, `\`, `\\`) + `"}`
	addReq := httptest.NewRequest(http.MethodPost, "/api/settings/sources", strings.NewReader(addBody))
	addReq.Header.Set("Content-Type", "application/json")
	addW := httptest.NewRecorder()
	srv.Handler().ServeHTTP(addW, addReq)
	if addW.Code != http.StatusOK {
		t.Fatalf("add returned %d; body=%s", addW.Code, addW.Body.String())
	}

	// Remove the newly-added source via the real HTTP handler.
	delReq := httptest.NewRequest(http.MethodDelete, "/api/settings/sources/"+addName, nil)
	delW := httptest.NewRecorder()
	srv.Handler().ServeHTTP(delW, delReq)
	if delW.Code != http.StatusOK {
		t.Fatalf("delete returned %d; body=%s", delW.Code, delW.Body.String())
	}

	// The pre-existing "e2e-tests" config entry must be completely untouched.
	srv.cfgMu.RLock()
	entry := srv.cfg.Spec.Entries["e2e-tests"]
	_, addStillPresent := srv.cfg.Spec.Entries[addName]
	srv.cfgMu.RUnlock()
	if entry == nil || entry.Ref == nil || entry.Ref.Path != tsPath {
		t.Fatalf("e2e-tests config entry was disturbed by the add/remove of %q: %+v", addName, entry)
	}
	if addStillPresent {
		t.Fatalf("%q config entry should have been removed by DELETE", addName)
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
	sourceMgr := NewSourceManager(initialCfg, map[string]*taskset.Source{"buildin": src}, nil, dir, zap.NewNop())
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

// newTestServerWithReconciler builds a Server wired to a real, running
// *registry.Reconciler — unlike newTestServer/newTestServerWithSourceMgr
// (server_test.go), which always construct with reconciler=nil, so the
// s.reconciler != nil branches in apiAddSource/apiRemoveSource (including
// reconcileClaimAfterAdd) are never exercised by any other test in this
// package. The reconciler is started with zero initial sources; sources can
// still be added dynamically via rec.AddSource, exactly as apiAddSource does.
//
// Unlike newTestServerWithSourceMgr, the trigger engine is built with a nil
// default executor instead of a real denoruntime.New(...) — this test never
// runs a task, only exercises the add/remove-source race, so it deliberately
// avoids denoruntime's "ensure deno binary is cached, else download it" step
// (which requires network access many sandboxes don't have) rather than
// t.Skipf-ing when it's unavailable.
func newTestServerWithReconciler(t *testing.T, cfg *config.Config) (*Server, *registry.Reconciler, *SourceManager) {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())

	rec := registry.NewReconciler(reg, nil, "", zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = rec.Run(ctx) }()
	// Run's goroutine must set rc.runCtx before any caller drives AddSource —
	// without this wait, a caller that races the goroutine's startup can hit
	// "reconciler not yet running" (pkg/registry/reconciler.go's startSource),
	// which was flaky enough to intermittently fail TestApiAddSource_* (#752).
	// The reconciler is always constructed with zero sources here, so Ready()
	// closes as soon as Run sets rc.runCtx — see Reconciler.Run's zero-source
	// branch — making this a precise, non-flaky gate rather than a sleep.
	<-rec.Ready()

	sourceMgr := NewSourceManager(cfg, nil, reg, t.TempDir(), zap.NewNop())

	srv, err := New(8080, reg, eng, cfg, "", nil, rec, sourceMgr, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())
	if err != nil {
		t.Fatalf("New server: %v", err)
	}
	sourceMgr.BindCfgMutex(&srv.cfgMu)
	return srv, rec, sourceMgr
}

// blockingSource is a source.Source whose Start blocks until the test closes
// unblock. It lets a test deterministically hold open the exact window
// pkg/registry/reconciler.go's startSource leaves between calling src.Start
// and populating rc.cancels[id] (cancels is only set *after* Start returns),
// instead of relying on real timing to hit that window.
//
// startedCtx is a buffered(1) channel that receives the per-source ctx
// (srcCtx) passed into Start, the moment Start is entered. Tests that need to
// observe whether the reconciler later cancelled this source (via
// RemoveSource, after Start already returned) can receive from startedCtx
// once and then check ctx.Err() — context.CancelFunc marks a context's Err()
// permanently once called, even if nothing is still selecting on Done(), so
// this is a reliable after-the-fact signal that cleanup ran.
type blockingSource struct {
	id         string
	unblock    chan struct{}
	ch         chan source.Event
	startedCtx chan context.Context
}

func newBlockingSource(id string) *blockingSource {
	return &blockingSource{
		id:         id,
		unblock:    make(chan struct{}),
		ch:         make(chan source.Event, 1),
		startedCtx: make(chan context.Context, 1),
	}
}

func (b *blockingSource) ID() string { return b.id }

func (b *blockingSource) Start(ctx context.Context) (<-chan source.Event, error) {
	select {
	case b.startedCtx <- ctx:
	default:
	}
	select {
	case <-b.unblock:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return b.ch, nil
}

func (b *blockingSource) Sync(context.Context) error { return nil }

// TestNewTestServerWithReconciler_AddSourceNeverRacesReconcilerStart is a
// regression test for #752: newTestServerWithReconciler used to return as
// soon as `go rec.Run(ctx)` was launched, without waiting for that goroutine
// to actually set rc.runCtx. A caller that immediately drove
// reconciler.AddSource — exactly what TestApiAddSource_ConcurrentRemove_NoOrphan
// and TestApiAddSource_ABA_ReAddDuringSlowAddSource_NoStaleClobber do — could
// then race the goroutine's startup and observe "reconciler not yet running"
// (pkg/registry/reconciler.go's startSource), reported failing about 1 run in
// 5. The helper now blocks on <-rec.Ready() before returning, so every caller
// gets an already-running reconciler.
//
// This drives that same immediate-AddSource pattern in a tight loop with no
// extra synchronization of its own, so it exercises exactly the ordering the
// helper is responsible for. Before the fix this failed intermittently within
// a small number of iterations (matching the issue's observed flake rate);
// with the fix it is deterministic, since Ready() closing is guaranteed to
// happen-after rc.runCtx is set for a reconciler with zero configured sources
// (Reconciler.Run's zero-source branch runs both under the same mutex
// section, before the goroutine can be observed by any other goroutine).
func TestNewTestServerWithReconciler_AddSourceNeverRacesReconcilerStart(t *testing.T) {
	for i := 0; i < 25; i++ {
		cfg := &config.Config{Server: config.ServerConfig{Port: 8080}}
		cfg.Spec.Entries = map[string]*taskset.Entry{}
		_, rec, _ := newTestServerWithReconciler(t, cfg)

		bs := newBlockingSource(fmt.Sprintf("ready-check-%d", i))
		close(bs.unblock) // Start returns immediately; only startup ordering is under test.
		if err := rec.AddSource(bs); err != nil {
			t.Fatalf("iteration %d: AddSource immediately after newTestServerWithReconciler returned: %v", i, err)
		}
	}
}

// TestApiAddSource_ConcurrentRemove_NoOrphan is a regression test for the
// race the 3fafc3f atomic-claim fix introduced between apiAddSource and
// apiRemoveSource:
//
//  1. apiAddSource claims cfg.Spec.Entries[name] under s.cfgMu, then calls
//     reconciler.AddSource(ts) — which calls ts.Start synchronously *before*
//     populating rc.cancels[id] (pkg/registry/reconciler.go's startSource).
//  2. While that Start call is still in flight, a concurrent apiRemoveSource
//     for the same name sees the already-claimed config entry, deletes it,
//     and calls reconciler.RemoveSource(id) — which misses (rc.cancels[id]
//     isn't populated yet) and is a silent no-op.
//  3. AddSource then finishes. Without the reconcileClaimAfterAdd guard,
//     apiAddSource would unconditionally register the source with
//     sourceMgr — leaving a live, registered source with no corresponding
//     cfg.Spec.Entries entry: an orphan, the exact failure mode the 3fafc3f
//     rollback was built to prevent, from the other direction.
//
// This drives the real *registry.Reconciler (not a mock) with a
// blockingSource to reproduce the ordering precondition deterministically
// instead of relying on real timing, then asserts the post-add state is
// always fully consistent: either the source is completely removed (no
// config entry, not registered with sourceMgr) or completely present
// (config entry AND registered) — never the split, orphaned state.
func TestApiAddSource_ConcurrentRemove_NoOrphan(t *testing.T) {
	const name = "race-add-remove"
	const id = "race-add-remove-id"

	cfg := &config.Config{Server: config.ServerConfig{Port: 8080}}
	cfg.Spec.Entries = map[string]*taskset.Entry{}

	srv, rec, sourceMgr := newTestServerWithReconciler(t, cfg)

	// Step 1: claim the config entry exactly as apiAddSource's updateConfig
	// callback does before calling reconciler.AddSource. claimedEntry mirrors
	// the `entry` variable apiAddSource keeps in scope for the identity check
	// in reconcileClaimAfterAdd.
	claimedEntry := &taskset.Entry{Ref: &taskset.Ref{Path: "/tmp/" + name}}
	if err := srv.updateConfig(func(cfg *config.Config) error {
		cfg.Spec.Entries[name] = claimedEntry
		return nil
	}); err != nil {
		t.Fatalf("claim entry: %v", err)
	}

	// Step 2: start reconciler.AddSource(bs) — the same call apiAddSource
	// makes — in the background. bs.Start blocks until unblocked, holding
	// open the window where rc.cancels[id] is not yet populated.
	bs := newBlockingSource(id)
	addDone := make(chan error, 1)
	go func() { addDone <- rec.AddSource(bs) }()

	// Give the AddSource goroutine a moment to actually enter bs.Start so
	// the RemoveSource-misses-and-no-ops step below reliably lands inside
	// the race window. (The regression assertions after unblocking do not
	// themselves depend on this timing — they hold regardless of when
	// apiRemoveSource's delete lands relative to bs.Start.)
	time.Sleep(20 * time.Millisecond)

	// Step 3: apiRemoveSource racing in concurrently — delete the config
	// entry and call reconciler.RemoveSource(id), reproducing exactly what
	// apiRemoveSource does. This call is expected to be a no-op teardown
	// (rc.cancels[id] not populated yet, since bs.Start is still blocked),
	// which is the precondition for the orphan bug.
	if err := srv.updateConfig(func(cfg *config.Config) error {
		delete(cfg.Spec.Entries, name)
		return nil
	}); err != nil {
		t.Fatalf("remove entry: %v", err)
	}
	rec.RemoveSource(id)

	// Step 4: let AddSource finish.
	close(bs.unblock)
	if err := <-addDone; err != nil {
		t.Fatalf("reconciler.AddSource: %v", err)
	}

	// Step 5: run the exact post-AddSource logic apiAddSource uses. The entry
	// claimed in Step 1 was deleted (never replaced) by Step 3, so
	// cfg.Spec.Entries[name] is nil here — reconcileClaimAfterAdd's identity
	// check (nil != claimedEntry) reports "lost the race" just like the old
	// name-presence check did for this scenario.
	if srv.reconcileClaimAfterAdd(name, id, claimedEntry) {
		sourceMgr.Register(name, taskset.NewSource(id, name, &taskset.Ref{Path: "/tmp/" + name}, "", t.TempDir(), false, 0, zap.NewNop()))
	}

	// Step 6: assert full consistency — the orphan state (registered but no
	// config entry) must never occur.
	srv.cfgMu.RLock()
	_, cfgPresent := srv.cfg.Spec.Entries[name]
	srv.cfgMu.RUnlock()
	_, mgrPresent := sourceMgr.Get(name)

	if mgrPresent != cfgPresent {
		t.Fatalf("orphan reproduced: sourceMgr registered=%v but cfg.Spec.Entries present=%v (name=%q) — want both true or both false",
			mgrPresent, cfgPresent, name)
	}
	if cfgPresent {
		t.Fatalf("concurrent apiRemoveSource deleted cfg.Spec.Entries[%q] first; reconcileClaimAfterAdd should not have let it come back, but cfg entry present=%v", name, cfgPresent)
	}
}

// TestApiAddSource_ABA_ReAddDuringSlowAddSource_NoStaleClobber is a
// regression test for the ABA race left behind by the fix that produced
// TestApiAddSource_ConcurrentRemove_NoOrphan above: comparing
// cfg.Spec.Entries[name] by NAME PRESENCE (both in the AddSource-failure
// rollback and in reconcileClaimAfterAdd) is not enough, because the slot can
// be reoccupied by a completely different *taskset.Entry between the claim
// and the re-check.
//
// Sequence reproduced here (matching the bug report):
//  1. Request A claims cfg.Spec.Entries[name] = entryA, then starts the slow
//     reconciler.AddSource(bsA) — bsA.Start blocks, holding open the window
//     before rc.cancels[idA] is populated (same precondition as the sibling
//     test above).
//  2. Request B (DELETE) races in while A's AddSource is in flight: sees
//     entryA present, deletes cfg.Spec.Entries[name], and calls
//     reconciler.RemoveSource(idA) — which misses (rc.cancels[idA] not
//     populated yet) and is a silent no-op, exactly like the sibling test.
//  3. Request C (POST) re-adds the now-free name: claims
//     cfg.Spec.Entries[name] = entryC (a brand-new pointer) and completes its
//     own reconciler.AddSource(tsC) + sourceMgr.Register(tsC) synchronously.
//  4. A's slow AddSource(bsA) finally returns. reconcileClaimAfterAdd for A
//     must detect that cfg.Spec.Entries[name] is now entryC, not entryA —
//     NOT treat "name present" as "still claimed" — self-clean bsA/tsA via
//     reconciler.RemoveSource(idA), and leave entryC/tsC completely alone.
//
// On the pre-fix (name-presence-only) code, step 4 would wrongly report
// "still claimed" (the name IS present — it's just entryC, not entryA), so
// apiAddSource would register the stale tsA into sourceMgr on top of C's
// tsC, and bsA's reconciler-side registration (rc.cancels[idA]) would never
// be cleaned up — a permanent orphan.
func TestApiAddSource_ABA_ReAddDuringSlowAddSource_NoStaleClobber(t *testing.T) {
	const name = "race-aba"
	const idA = "race-aba-id-a"
	const idC = "race-aba-id-c"

	cfg := &config.Config{Server: config.ServerConfig{Port: 8080}}
	cfg.Spec.Entries = map[string]*taskset.Entry{}

	srv, rec, sourceMgr := newTestServerWithReconciler(t, cfg)

	// Step 1: Request A claims cfg.Spec.Entries[name] = entryA, mirroring the
	// `entry` variable apiAddSource keeps in scope for its identity checks.
	entryA := &taskset.Entry{Ref: &taskset.Ref{Path: "/tmp/" + idA}}
	if err := srv.updateConfig(func(cfg *config.Config) error {
		cfg.Spec.Entries[name] = entryA
		return nil
	}); err != nil {
		t.Fatalf("claim entryA: %v", err)
	}

	// Step 2: Request A starts reconciler.AddSource(bsA) in the background;
	// bsA.Start blocks, holding open the window before rc.cancels[idA] is
	// populated.
	bsA := newBlockingSource(idA)
	addADone := make(chan error, 1)
	go func() { addADone <- rec.AddSource(bsA) }()

	// Give the AddSource goroutine a moment to actually enter bsA.Start so
	// the RemoveSource-misses-and-no-ops step below reliably lands inside the
	// race window.
	time.Sleep(20 * time.Millisecond)

	// Step 3: Request B (DELETE) races in: deletes cfg.Spec.Entries[name]
	// (currently entryA) and calls reconciler.RemoveSource(idA) — a no-op
	// since rc.cancels[idA] isn't populated yet (bsA.Start is still
	// blocked), reproducing exactly what apiRemoveSource does.
	if err := srv.updateConfig(func(cfg *config.Config) error {
		delete(cfg.Spec.Entries, name)
		return nil
	}); err != nil {
		t.Fatalf("remove entryA: %v", err)
	}
	rec.RemoveSource(idA)

	// Step 4: Request C (POST) re-adds "name" — the slot is free again — and
	// claims a brand-new *taskset.Entry (entryC). Its underlying source
	// (bsC) does not block, so reconciler.AddSource(bsC) and
	// sourceMgr.Register both complete synchronously here, exactly as
	// apiAddSource does end-to-end for a fast, non-racing request.
	entryC := &taskset.Entry{Ref: &taskset.Ref{Path: "/tmp/" + idC}}
	if err := srv.updateConfig(func(cfg *config.Config) error {
		if _, exists := cfg.Spec.Entries[name]; exists {
			t.Fatalf("expected name %q to be free before C's claim (B's delete should have landed)", name)
		}
		cfg.Spec.Entries[name] = entryC
		return nil
	}); err != nil {
		t.Fatalf("claim entryC: %v", err)
	}
	bsC := newBlockingSource(idC)
	close(bsC.unblock) // C's Start never actually blocks; only A holds the race window
	if err := rec.AddSource(bsC); err != nil {
		t.Fatalf("reconciler.AddSource(bsC): %v", err)
	}
	tsC := taskset.NewSource(idC, name, entryC.Ref, "", t.TempDir(), false, 0, zap.NewNop())
	if !srv.reconcileClaimAfterAdd(name, idC, entryC) {
		t.Fatalf("C's own claim should still be standing immediately after C claims and registers it")
	}
	sourceMgr.Register(name, tsC)

	// Step 5: A's slow AddSource finally returns.
	close(bsA.unblock)
	if err := <-addADone; err != nil {
		t.Fatalf("reconciler.AddSource(bsA): %v", err)
	}

	// Step 6: run apiAddSource's post-AddSource logic for request A. This is
	// the exact call the bug report identifies: on the pre-fix, name-presence
	// -only code this wrongly reports "still claimed" (cfg.Spec.Entries[name]
	// exists — it's just entryC now, not entryA) and would register the
	// stale tsA into sourceMgr, clobbering/racing with C's already-registered
	// tsC. The fix must compare identity (entryA) against what's actually in
	// the slot (entryC) and treat A as having lost the race.
	tsA := taskset.NewSource(idA, name, entryA.Ref, "", t.TempDir(), false, 0, zap.NewNop())
	if srv.reconcileClaimAfterAdd(name, idA, entryA) {
		sourceMgr.Register(name, tsA)
	}

	// Step 7: assert the end state reflects ONLY C's source, never A's stale
	// one.
	got, ok := sourceMgr.Get(name)
	if !ok {
		t.Fatalf("expected sourceMgr to have an entry for %q", name)
	}
	if got.ID() != idC {
		t.Fatalf("sourceMgr[%q] has id %q, want C's id %q — stale A must not clobber C's registration", name, got.ID(), idC)
	}

	// cfg.Spec.Entries[name] must still be C's entry — A's belated
	// reconcileClaimAfterAdd call must not have touched it.
	srv.cfgMu.RLock()
	finalEntry := srv.cfg.Spec.Entries[name]
	srv.cfgMu.RUnlock()
	if finalEntry != entryC {
		t.Fatalf("cfg.Spec.Entries[%q] was mutated by A's losing claim; want it untouched at entryC", name)
	}

	// A's reconciler-side registration must have been cleaned up (via
	// reconciler.RemoveSource(idA) inside reconcileClaimAfterAdd), not left
	// as a permanent orphan. bsA's captured srcCtx will have been cancelled
	// if and only if that cleanup ran — RemoveSource looks up rc.cancels[idA]
	// (now populated, since AddSource(bsA) has returned) and invokes its
	// CancelFunc, which permanently marks the context's Err() even though
	// bsA.Start has already returned and nothing is still selecting on
	// Done().
	select {
	case srcCtx := <-bsA.startedCtx:
		if srcCtx.Err() == nil {
			t.Fatalf("bsA's per-source context was never cancelled after losing the ABA race; reconciler.RemoveSource(idA) should have run — orphaned source leak")
		}
	case <-time.After(time.Second):
		t.Fatalf("bsA.Start was never observed to run")
	}
}

// TestSourceManager_Sources_StripsURLCredentials pins the one field in the
// listing that can carry a secret. permissions.dicode.sources_list is granted
// to agent tasks, so a PAT embedded in a configured source URL — the shape
// pkg/taskset/sanitize.go documents as routine — would otherwise be readable
// by a language model through the MCP list_sources tool.
func TestSourceManager_Sources_StripsURLCredentials(t *testing.T) {
	const secret = "ghp_supersecrettoken"
	mgr := &SourceManager{
		cfg: &config.Config{Spec: taskset.TaskSetBody{Entries: map[string]*taskset.Entry{
			"private": {Ref: &taskset.Ref{
				URL:    "https://oauth2:" + secret + "@github.com/org/repo.git",
				Branch: "main",
			}},
		}}},
	}

	got := mgr.Sources()
	if len(got) != 1 {
		t.Fatalf("expected one summary, got %+v", got)
	}
	if strings.Contains(got[0].URL, secret) {
		t.Errorf("source listing leaks the URL credential: %q", got[0].URL)
	}
	if got[0].URL != "https://github.com/org/repo.git" {
		t.Errorf("URL = %q, want the repo without userinfo", got[0].URL)
	}
}

// TestApiAddSource_PinnedTag adds a source pinned to a tag and verifies the
// entry carries the pin without the "main" branch every unpinned git source
// gets.
func TestApiAddSource_PinnedTag(t *testing.T) {
	srv, _ := newTestServerWithConfigPath(t)

	body := `{"name":"pinned","url":"https://github.com/example/repo.git","tag":"v1.2.3"}`
	req := httptest.NewRequest(http.MethodPost, "/api/settings/sources", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/settings/sources returned %d; body=%s", w.Code, w.Body.String())
	}

	srv.cfgMu.RLock()
	entry := srv.cfg.Spec.Entries["pinned"]
	srv.cfgMu.RUnlock()
	if entry == nil || entry.Ref == nil {
		t.Fatal("expected pinned entry in config after add")
	}
	if entry.Ref.Tag != "v1.2.3" {
		t.Errorf("tag = %q, want v1.2.3", entry.Ref.Tag)
	}
	if entry.Ref.Branch != "" {
		t.Errorf("branch = %q on a pinned source, want it left unset", entry.Ref.Branch)
	}
}

// TestApiAddSource_RejectsBadTarget keeps the handler under the same rules
// config-load applies: a request it accepted but the loader rejects would
// leave a dicode.yaml the daemon cannot boot from.
func TestApiAddSource_RejectsBadTarget(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"branch and tag together", `{"name":"both","url":"https://github.com/example/repo.git","branch":"main","tag":"v1.2.3"}`},
		{"malformed tag", `{"name":"bad","url":"https://github.com/example/repo.git","tag":"v1..3"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newTestServerWithConfigPath(t)
			req := httptest.NewRequest(http.MethodPost, "/api/settings/sources", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("got %d; want 400; body=%s", w.Code, w.Body.String())
			}
			srv.cfgMu.RLock()
			_, added := srv.cfg.Spec.Entries["both"]
			_, added2 := srv.cfg.Spec.Entries["bad"]
			srv.cfgMu.RUnlock()
			if added || added2 {
				t.Error("a rejected add still claimed an entry in spec.entries")
			}
		})
	}
}
