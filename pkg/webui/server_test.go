package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	jsruntime "github.com/dicode/dicode/pkg/runtime/js"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/trigger"
	"go.uber.org/zap"
)

func newTestServer(t *testing.T) (*Server, *registry.Registry) {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	reg := registry.New(d)
	rt := jsruntime.New(reg, secrets.Chain{}, d, zap.NewNop())
	eng := trigger.New(reg, rt, zap.NewNop())

	srv, err := New(8080, reg, eng, &config.Config{Server: config.ServerConfig{Port: 8080}}, NewLogBroadcaster(), zap.NewNop())
	if err != nil {
		t.Fatalf("New server: %v", err)
	}
	return srv, reg
}

func registerTask(t *testing.T, reg *registry.Registry, id, script string) *task.Spec {
	t.Helper()
	dir := t.TempDir()
	td := filepath.Join(dir, id)
	_ = os.MkdirAll(td, 0755)
	_ = os.WriteFile(filepath.Join(td, "task.yaml"), []byte("name: "+id+"\ntrigger:\n  manual: true\nruntime: js\n"), 0644)
	_ = os.WriteFile(filepath.Join(td, "task.js"), []byte(script), 0644)
	spec := &task.Spec{
		ID:      id,
		Name:    id,
		Runtime: task.RuntimeJS,
		Trigger: task.TriggerConfig{Manual: true},
		Timeout: 5 * time.Second,
		TaskDir: td,
	}
	_ = reg.Register(spec)
	return spec
}

func TestAPI_ListTasks(t *testing.T) {
	srv, reg := newTestServer(t)
	registerTask(t, reg, "task-a", `return 1`)
	registerTask(t, reg, "task-b", `return 2`)

	req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var tasks []map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&tasks); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(tasks) != 2 {
		t.Errorf("expected 2 tasks, got %d", len(tasks))
	}
}

func TestAPI_GetTask(t *testing.T) {
	srv, reg := newTestServer(t)
	registerTask(t, reg, "my-task", `return 1`)

	req := httptest.NewRequest(http.MethodGet, "/api/tasks/my-task", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body)
	}
}

func TestAPI_GetTask_NotFound(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/tasks/missing", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestAPI_RunTask(t *testing.T) {
	srv, reg := newTestServer(t)
	registerTask(t, reg, "run-me", `return "done"`)

	req := httptest.NewRequest(http.MethodPost, "/api/tasks/run-me/run", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body)
	}
	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["runId"] == "" {
		t.Error("expected runId in response")
	}
}

func TestAPI_RunTask_NotFound(t *testing.T) {
	srv, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/ghost/run", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestAPI_ListRuns(t *testing.T) {
	srv, reg := newTestServer(t)
	registerTask(t, reg, "history-task", `return 1`)

	// Fire a run via the API.
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/history-task/run", nil)
	srv.Handler().ServeHTTP(httptest.NewRecorder(), req)

	req2 := httptest.NewRequest(http.MethodGet, "/api/tasks/history-task/runs", nil)
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w2.Code)
	}
	var runs []map[string]interface{}
	_ = json.NewDecoder(w2.Body).Decode(&runs)
	if len(runs) == 0 {
		t.Error("expected at least one run")
	}
}

func TestAPI_GetRun_and_Logs(t *testing.T) {
	srv, reg := newTestServer(t)
	registerTask(t, reg, "log-task", `log.info("hello"); return 1`)

	// Fire a run.
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/log-task/run", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	var resp map[string]string
	_ = json.NewDecoder(w.Body).Decode(&resp)
	runID := resp["runId"]

	// Get run.
	req2 := httptest.NewRequest(http.MethodGet, "/api/runs/"+runID, nil)
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("get run: %d", w2.Code)
	}

	// Get logs.
	req3 := httptest.NewRequest(http.MethodGet, "/api/runs/"+runID+"/logs", nil)
	w3 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w3, req3)
	if w3.Code != http.StatusOK {
		t.Fatalf("get logs: %d", w3.Code)
	}
	var logs []map[string]interface{}
	_ = json.NewDecoder(w3.Body).Decode(&logs)
	if len(logs) == 0 {
		t.Error("expected log entries")
	}
}

func TestUI_TaskList(t *testing.T) {
	srv, reg := newTestServer(t)
	registerTask(t, reg, "ui-task", `return 1`)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !contains(body, "ui-task") {
		t.Error("task not visible in UI")
	}
}

func TestUI_TaskDetail(t *testing.T) {
	srv, reg := newTestServer(t)
	registerTask(t, reg, "detail-task", `return 1`)

	req := httptest.NewRequest(http.MethodGet, "/tasks/detail-task", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestUI_RunDetail(t *testing.T) {
	srv, reg := newTestServer(t)
	registerTask(t, reg, "rundetail-task", `return 1`)

	// Create a run.
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/rundetail-task/run", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	var resp map[string]string
	_ = json.NewDecoder(w.Body).Decode(&resp)

	req2 := httptest.NewRequest(http.MethodGet, "/runs/"+resp["runId"], nil)
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w2.Code, w2.Body)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
