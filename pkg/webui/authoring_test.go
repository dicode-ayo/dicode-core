package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/taskset"
	"github.com/dicode/dicode/pkg/trigger"
	"go.uber.org/zap"
)

// newAuthoringTestServer builds a Server with auth disabled, a real SQLite DB,
// and a SourceManager wired up with a local taskset source rooted at dir.
func newAuthoringTestServer(t *testing.T, sourceName, sourceDir string) *Server {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	cfg := &config.Config{}

	var sm *SourceManager
	if sourceName != "" && sourceDir != "" {
		sm = NewSourceManager(cfg, nil, reg, "", zap.NewNop())
		// Create a minimal taskset source. We only need RepoPath() to return
		// the dir so the create handler can write files.
		src := newStubTasksetSource(t, sourceName, sourceDir)
		sm.Register(sourceName, src)
	}

	srv, err := New(0, reg, eng, cfg, "", nil, nil, sm, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())
	if err != nil {
		t.Fatalf("webui.New: %v", err)
	}
	return srv
}

// newStubTasksetSource creates a *taskset.Source backed by a local directory.
// The source is not started (no fsnotify), but RepoPath() returns the dir.
func newStubTasksetSource(t *testing.T, name, dir string) *taskset.Source {
	t.Helper()
	// Write a minimal taskset.yaml so taskset.NewSource doesn't fail.
	tsYAML := filepath.Join(dir, "taskset.yaml")
	if err := os.WriteFile(tsYAML, []byte("apiVersion: dicode/v1\nkind: TaskSet\nspec:\n  entries: {}\n"), 0644); err != nil {
		t.Fatalf("write taskset.yaml: %v", err)
	}
	ref := &taskset.Ref{Path: tsYAML}
	src := taskset.NewSource(name, name, ref, "", "", false, 0, zap.NewNop())

	// Start the source so RepoPath() returns the directory. For a local ref
	// this just sets watchRoot = dir and runs an initial resolve.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if _, err := src.Start(ctx); err != nil {
		t.Fatalf("start source: %v", err)
	}

	return src
}

func postJSON(handler http.Handler, path string, body any) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func decodeJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(w.Body).Decode(&m); err != nil {
		t.Fatalf("decode JSON: %v (body: %s)", err, w.Body.String())
	}
	return m
}

// --- POST /api/task/create --------------------------------------------------

func TestAPITaskCreate_HappyPath(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "ai-scratch", dir)
	h := srv.Handler()

	w := postJSON(h, "/api/task/create", map[string]string{"name": "My Cool Task"})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", w.Code, w.Body.String())
	}

	body := decodeJSON(t, w)
	taskID, _ := body["task_id"].(string)
	if taskID != "ai-scratch/my-cool-task" {
		t.Errorf("task_id = %q, want %q", taskID, "ai-scratch/my-cool-task")
	}
	files, _ := body["files"].([]any)
	if len(files) != 2 {
		t.Errorf("files = %v, want 2 entries", files)
	}

	// Verify files on disk.
	if _, err := os.Stat(filepath.Join(dir, "my-cool-task", "task.yaml")); err != nil {
		t.Errorf("task.yaml not found on disk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "my-cool-task", "task.js")); err != nil {
		t.Errorf("task.js not found on disk: %v", err)
	}
}

// Scaffolding files is only half the job: the source resolves through
// spec.entries, so an unlisted directory never becomes a task.
func TestAPITaskCreate_ScaffoldedTaskResolves(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "ai-scratch", dir)

	w := postJSON(srv.Handler(), "/api/task/create", map[string]string{"name": "regcheck"})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", w.Code, w.Body.String())
	}

	tsPath := filepath.Join(dir, "taskset.yaml")
	resolver := taskset.NewResolver(t.TempDir(), false, zap.NewNop())
	tasks, failures, err := resolver.Resolve(context.Background(), "ai-scratch", &taskset.Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("resolve source: %v", err)
	}
	if len(failures) != 0 {
		t.Fatalf("resolve failures: %v", failures)
	}
	if len(tasks) != 1 || tasks[0].ID != "ai-scratch/regcheck" {
		t.Fatalf("resolved %d tasks (%v), want ai-scratch/regcheck", len(tasks), tasks)
	}
}

func TestAPITaskCreate_ExistingTaskConflicts(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "ai-scratch", dir)
	h := srv.Handler()

	if w := postJSON(h, "/api/task/create", map[string]string{"name": "regcheck"}); w.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201; body: %s", w.Code, w.Body.String())
	}
	// Rewrite the script so a clobbering second create would be visible.
	scriptPath := filepath.Join(dir, "regcheck", "task.js")
	if err := os.WriteFile(scriptPath, []byte("// authored\n"), 0o644); err != nil {
		t.Fatalf("write task.js: %v", err)
	}

	w := postJSON(h, "/api/task/create", map[string]string{"name": "regcheck"})
	if w.Code != http.StatusConflict {
		t.Fatalf("second create status = %d, want 409; body: %s", w.Code, w.Body.String())
	}
	script, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read task.js: %v", err)
	}
	if string(script) != "// authored\n" {
		t.Errorf("task.js was overwritten: %q", script)
	}
}

func TestAPITaskCreate_AutoName(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "ai-scratch", dir)
	h := srv.Handler()

	w := postJSON(h, "/api/task/create", map[string]string{})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", w.Code, w.Body.String())
	}

	body := decodeJSON(t, w)
	taskID, _ := body["task_id"].(string)
	if taskID == "" {
		t.Error("task_id should not be empty")
	}
}

func TestAPITaskCreate_SourceNotFound(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "ai-scratch", dir)
	h := srv.Handler()

	w := postJSON(h, "/api/task/create", map[string]string{"source": "nonexistent"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
}

func TestAPITaskCreate_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "ai-scratch", dir)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodPost, "/api/task/create", bytes.NewReader([]byte("not json")))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// --- POST /api/task/edit ----------------------------------------------------

func TestAPITaskEdit_CreateSession(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "ai-scratch", dir)
	h := srv.Handler()

	w := postJSON(h, "/api/task/edit", map[string]string{"task_id": "ai-scratch/hello"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body: %s", w.Code, w.Body.String())
	}

	body := decodeJSON(t, w)
	sessID, _ := body["session_id"].(string)
	if sessID == "" {
		t.Error("session_id should not be empty")
	}
	src, _ := body["source"].(string)
	if src != "ai-scratch" {
		t.Errorf("source = %q, want %q", src, "ai-scratch")
	}
}

func TestAPITaskEdit_ResumeSession(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "ai-scratch", dir)
	h := srv.Handler()

	// Create a session first.
	w := postJSON(h, "/api/task/edit", map[string]string{"task_id": "ai-scratch/hello"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	body := decodeJSON(t, w)
	sessID := body["session_id"].(string)

	// Resume it.
	w = postJSON(h, "/api/task/edit", map[string]string{"session_id": sessID})
	if w.Code != http.StatusAccepted {
		t.Fatalf("resume status = %d, want 202; body: %s", w.Code, w.Body.String())
	}
	body2 := decodeJSON(t, w)
	if body2["session_id"] != sessID {
		t.Errorf("resumed session_id = %q, want %q", body2["session_id"], sessID)
	}
}

func TestAPITaskEdit_SameTaskAutoResumes(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "ai-scratch", dir)
	h := srv.Handler()

	// First edit opens a session for the task.
	w := postJSON(h, "/api/task/edit", map[string]string{"task_id": "ai-scratch/hello"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202; body: %s", w.Code, w.Body.String())
	}
	first := decodeJSON(t, w)
	sessID, _ := first["session_id"].(string)
	if sessID == "" {
		t.Fatal("first edit returned empty session_id")
	}

	// A second edit for the SAME task id auto-resumes the open session rather
	// than rejecting with 409 — pins the #288 same-task contract.
	w = postJSON(h, "/api/task/edit", map[string]string{"task_id": "ai-scratch/hello"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("same-task status = %d, want 202 auto-resume; body: %s", w.Code, w.Body.String())
	}
	second := decodeJSON(t, w)
	if second["session_id"] != sessID {
		t.Errorf("auto-resume session_id = %v, want same %q", second["session_id"], sessID)
	}
}

func TestAPITaskEdit_DifferentTaskSameSourceConflicts(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "ai-scratch", dir)
	h := srv.Handler()

	w := postJSON(h, "/api/task/edit", map[string]string{"task_id": "ai-scratch/hello"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202", w.Code)
	}

	w = postJSON(h, "/api/task/edit", map[string]string{"task_id": "ai-scratch/other"})
	if w.Code != http.StatusConflict {
		t.Fatalf("different-task status = %d, want 409; body: %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.Contains(body, "#283") {
		t.Errorf("409 body = %q, want #283 reference", body)
	}
}

func TestAPITaskEdit_ConflictSameSource(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "ai-scratch", dir)
	h := srv.Handler()

	// Create first session.
	w := postJSON(h, "/api/task/edit", map[string]string{"task_id": "ai-scratch/hello"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("first status = %d, want 202", w.Code)
	}

	// Try second session on same source.
	w = postJSON(h, "/api/task/edit", map[string]string{"task_id": "ai-scratch/other"})
	if w.Code != http.StatusConflict {
		t.Fatalf("second status = %d, want 409; body: %s", w.Code, w.Body.String())
	}
}

func TestAPITaskEdit_ResumeRejectsTaskMismatch(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "ai-scratch", dir)
	h := srv.Handler()

	// Open a session for ai-scratch/hello.
	w := postJSON(h, "/api/task/edit", map[string]string{"task_id": "ai-scratch/hello"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("open status = %d, want 202", w.Code)
	}
	sessID := decodeJSON(t, w)["session_id"].(string)

	// Resuming with a task id that does not match the session's must be
	// rejected rather than silently editing the wrong task.
	w = postJSON(h, "/api/task/edit", map[string]string{
		"session_id": sessID,
		"task_id":    "ai-scratch/wrong",
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("mismatch status = %d, want 409; body: %s", w.Code, w.Body.String())
	}

	// The same session id resumes cleanly when the task id matches.
	w = postJSON(h, "/api/task/edit", map[string]string{
		"session_id": sessID,
		"task_id":    "ai-scratch/hello",
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("matching-task resume status = %d, want 202; body: %s", w.Code, w.Body.String())
	}
}

func TestAPITaskEdit_MissingTaskAndSession(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "ai-scratch", dir)
	h := srv.Handler()

	w := postJSON(h, "/api/task/edit", map[string]string{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

func TestAPITaskEdit_SessionNotFound(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "ai-scratch", dir)
	h := srv.Handler()

	w := postJSON(h, "/api/task/edit", map[string]string{"session_id": "nonexistent"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
}

// --- POST /api/task/save ----------------------------------------------------

func TestAPITaskSave_HappyPath(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "ai-scratch", dir)
	h := srv.Handler()

	// Create a session.
	w := postJSON(h, "/api/task/edit", map[string]string{"task_id": "ai-scratch/hello"})
	body := decodeJSON(t, w)
	sessID := body["session_id"].(string)

	// Save it.
	w = postJSON(h, "/api/task/save", map[string]string{"session_id": sessID})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body = decodeJSON(t, w)
	if body["applied"] != true {
		t.Errorf("applied = %v, want true", body["applied"])
	}
}

func TestAPITaskSave_SessionNotFound(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "ai-scratch", dir)
	h := srv.Handler()

	w := postJSON(h, "/api/task/save", map[string]string{"session_id": "nonexistent"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
}

func TestAPITaskSave_MissingSessionID(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "ai-scratch", dir)
	h := srv.Handler()

	w := postJSON(h, "/api/task/save", map[string]string{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

func TestAPITaskSave_AlreadyClosed(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "ai-scratch", dir)
	h := srv.Handler()

	// Create and save.
	w := postJSON(h, "/api/task/edit", map[string]string{"task_id": "ai-scratch/hello"})
	body := decodeJSON(t, w)
	sessID := body["session_id"].(string)
	postJSON(h, "/api/task/save", map[string]string{"session_id": sessID})

	// Save again.
	w = postJSON(h, "/api/task/save", map[string]string{"session_id": sessID})
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body: %s", w.Code, w.Body.String())
	}
}

// --- POST /api/task/cancel --------------------------------------------------

func TestAPITaskCancel_HappyPath(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "ai-scratch", dir)
	h := srv.Handler()

	// Create a session.
	w := postJSON(h, "/api/task/edit", map[string]string{"task_id": "ai-scratch/hello"})
	body := decodeJSON(t, w)
	sessID := body["session_id"].(string)

	// Cancel it.
	w = postJSON(h, "/api/task/cancel", map[string]string{"session_id": sessID})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
	body = decodeJSON(t, w)
	if body["cancelled"] != true {
		t.Errorf("cancelled = %v, want true", body["cancelled"])
	}
}

func TestAPITaskCancel_Idempotent(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "ai-scratch", dir)
	h := srv.Handler()

	// Create and cancel.
	w := postJSON(h, "/api/task/edit", map[string]string{"task_id": "ai-scratch/hello"})
	body := decodeJSON(t, w)
	sessID := body["session_id"].(string)
	postJSON(h, "/api/task/cancel", map[string]string{"session_id": sessID})

	// Cancel again — should be idempotent.
	w = postJSON(h, "/api/task/cancel", map[string]string{"session_id": sessID})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
	}
}

func TestAPITaskCancel_SessionNotFound(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "ai-scratch", dir)
	h := srv.Handler()

	w := postJSON(h, "/api/task/cancel", map[string]string{"session_id": "nonexistent"})
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
}

func TestAPITaskCancel_MissingSessionID(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "ai-scratch", dir)
	h := srv.Handler()

	w := postJSON(h, "/api/task/cancel", map[string]string{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

// --- sanitizeTaskName -------------------------------------------------------

func TestSanitizeTaskName(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"My Cool Task", "my-cool-task"},
		{"hello_world", "helloworld"},
		{"--abc--", "abc"},
		{"123", "123"},
		{"", ""},
		{"  spaces  ", "spaces"},
	}
	for _, tc := range tests {
		got := sanitizeTaskName(tc.in)
		if got != tc.want {
			t.Errorf("sanitizeTaskName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
