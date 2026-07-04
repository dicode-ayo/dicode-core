package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
)

// registerTaskWithDir registers a minimal deno task whose TaskDir points at
// dir, so apiGetTask's script-file discovery runs against real files.
func registerTaskWithDir(t *testing.T, reg *registry.Registry, id, dir string) {
	t.Helper()
	spec := &task.Spec{
		ID:      id,
		Name:    id,
		Runtime: task.RuntimeDeno,
		TaskDir: dir,
		Trigger: task.TriggerConfig{Manual: true},
		Enabled: true,
	}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
}

func getTaskDetail(t *testing.T, srv *Server, id string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/tasks/"+id, nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var detail map[string]any
	if err := json.NewDecoder(w.Body).Decode(&detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return detail
}

// TestAPI_GetTask_ScriptFile_RegularFile confirms the baseline: a real
// task.ts is reported as the script file.
func TestAPI_GetTask_ScriptFile_RegularFile(t *testing.T) {
	srv, reg, _ := newApprovalTestServer(t, false)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "task.ts"), []byte("// ok"), 0o644); err != nil {
		t.Fatalf("write task.ts: %v", err)
	}
	registerTaskWithDir(t, reg, "regular-task", dir)

	detail := getTaskDetail(t, srv, "regular-task")
	if got := detail["script_file"]; got != "task.ts" {
		t.Errorf("script_file = %v, want task.ts", got)
	}
}

// TestAPI_GetTask_ScriptFile_SymlinkRejected proves apiGetTask agrees with
// Spec.ScriptPath's symlink policy: a symlinked task.ts must NOT be reported
// as the script (the runtime refuses to execute it), so discovery skips it
// and reports the real task.js instead. Before the ScriptPath unification the
// hand-rolled fsutil.Exists loop reported the symlinked task.ts here.
func TestAPI_GetTask_ScriptFile_SymlinkRejected(t *testing.T) {
	srv, reg, _ := newApprovalTestServer(t, false)

	outside := t.TempDir()
	target := filepath.Join(outside, "evil.ts")
	if err := os.WriteFile(target, []byte("// outside the task dir"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}

	dir := t.TempDir()
	if err := os.Symlink(target, filepath.Join(dir, "task.ts")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.js"), []byte("// real"), 0o644); err != nil {
		t.Fatalf("write task.js: %v", err)
	}
	registerTaskWithDir(t, reg, "symlink-task", dir)

	detail := getTaskDetail(t, srv, "symlink-task")
	if got := detail["script_file"]; got != "task.js" {
		t.Errorf("script_file = %v, want task.js (symlinked task.ts must be rejected)", got)
	}
}

// TestAPI_GetTask_ScriptFile_NoScriptFallback keeps the historical default:
// when no acceptable script exists (here: only a rejected symlink), the
// detail still reports "task.ts" so the editor UI has a filename to offer.
func TestAPI_GetTask_ScriptFile_NoScriptFallback(t *testing.T) {
	srv, reg, _ := newApprovalTestServer(t, false)

	outside := t.TempDir()
	target := filepath.Join(outside, "evil.ts")
	if err := os.WriteFile(target, []byte("// outside"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}

	dir := t.TempDir()
	if err := os.Symlink(target, filepath.Join(dir, "task.ts")); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	registerTaskWithDir(t, reg, "fallback-task", dir)

	detail := getTaskDetail(t, srv, "fallback-task")
	if got := detail["script_file"]; got != "task.ts" {
		t.Errorf("script_file = %v, want task.ts fallback", got)
	}
}
