package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/tasktest"
)

// registerTaskWithTest writes a task.yaml + task.ts + task.test.ts under a
// temp dir and registers the resulting spec. The test file is whatever the
// caller hands in so different scenarios can pass/fail/hang as needed.
//
// The task carries a single optional string param "label" so the validator
// path has something to chew on in the schema-mismatch test.
func registerTaskWithTest(t *testing.T, reg *registry.Registry, id, taskScript, testScript string) *task.Spec {
	t.Helper()
	dir := t.TempDir()
	td := filepath.Join(dir, id)
	if err := os.MkdirAll(td, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	yaml := "name: " + id + "\n" +
		"trigger:\n  manual: true\n" +
		"runtime: deno\n" +
		"params:\n  label:\n    type: string\n    description: optional label\n"
	if err := os.WriteFile(filepath.Join(td, "task.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatalf("write task.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(td, "task.ts"), []byte(taskScript), 0644); err != nil {
		t.Fatalf("write task.ts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(td, "task.test.ts"), []byte(testScript), 0644); err != nil {
		t.Fatalf("write task.test.ts: %v", err)
	}
	spec := &task.Spec{
		ID:      id,
		Name:    id,
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true},
		Timeout: 5 * time.Second,
		TaskDir: td,
		Params: task.Params{
			{Name: "label", Type: "string"},
		},
	}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("register: %v", err)
	}
	return spec
}

// passingTest is the canonical Deno test fixture used by the success path.
// Kept short so test runs stay snappy.
const passingTest = `import { assertEquals } from "jsr:@std/assert@1";
Deno.test("ok", () => { assertEquals(1, 1); });
`

// hangingTest spins forever so the timeout path can demonstrate cancellation.
// Uses a chained setTimeout sleep loop rather than a single never-resolving
// promise so Deno's "unresolved promise" detector doesn't short-circuit and
// exit cleanly before the parent context cancels — that would defeat the
// timeout assertion. Each tick stays inside a real timer so Deno keeps the
// event loop alive past its self-check.
const hangingTest = `Deno.test("hang", async () => {
  while (true) {
    await new Promise((r) => setTimeout(r, 100));
  }
});
`

// TestAPI_TestTask_Success verifies that a happy-path POST to the test
// endpoint returns 200 with status="passed", a non-empty run_id, and the
// expected counts parsed out of the runner's summary line.
func TestAPI_TestTask_Success(t *testing.T) {
	srv, reg := newTestServer(t)
	registerTaskWithTest(t, reg, "ok-task", "", passingTest)

	req := httptest.NewRequest(http.MethodPost, "/api/tasks/ok-task/test", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp testTaskResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "passed" {
		t.Errorf("status: got %q, want passed (output: %s)", resp.Status, resp.Stdout)
	}
	if resp.RunID == "" {
		t.Error("run_id must be non-empty")
	}
	if resp.Passed < 1 {
		t.Errorf("passed: got %d, want >= 1", resp.Passed)
	}
}

// TestAPI_TestTask_NotFound covers the 404 path: an unknown task ID must
// return JSON {"error": ...} with HTTP 404, never 200.
func TestAPI_TestTask_NotFound(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/tasks/nope/test", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

// TestAPI_TestTask_MissingAPIKey covers the 401 path. requireAPIKey only
// activates when cfg.Server.Auth is true, so we use newAuthServer.
func TestAPI_TestTask_MissingAPIKey(t *testing.T) {
	srv := newAuthServer(t, "hunter2")

	req := httptest.NewRequest(http.MethodPost, "/api/tasks/anything/test", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	if got := w.Header().Get("WWW-Authenticate"); got == "" {
		t.Errorf("WWW-Authenticate must be set on 401, got empty")
	}
}

// TestAPI_TestTask_BadAPIKey verifies that a non-empty but unrecognized
// Bearer token still results in 401 (not silent passthrough).
func TestAPI_TestTask_BadAPIKey(t *testing.T) {
	srv := newAuthServer(t, "hunter2")

	req := httptest.NewRequest(http.MethodPost, "/api/tasks/anything/test", nil)
	req.Header.Set("Authorization", "Bearer dck_not-a-real-key")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

// TestAPI_TestTask_GoodAPIKey verifies the 401-then-200 path: an API key
// freshly generated via apiKeys.generate authenticates the same request that
// previously returned 401. We only assert that the request makes it past the
// auth wall (not 401, not 403); the body itself can be 404 because the
// in-memory registry is empty under newAuthServer.
func TestAPI_TestTask_GoodAPIKey(t *testing.T) {
	srv := newAuthServer(t, "hunter2")
	raw, _, err := srv.apiKeys.generate(context.Background(), "test-fixture")
	if err != nil {
		t.Fatalf("generate api key: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/tasks/anything/test", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code == http.StatusUnauthorized {
		t.Fatalf("valid bearer rejected: %d body=%s", w.Code, w.Body.String())
	}
	// The fixture registry has no tasks, so 404 is the expected 'past the
	// auth wall' signal here.
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (no such task) past auth, got %d: %s", w.Code, w.Body.String())
	}
}

// TestAPI_TestTask_SchemaMismatch covers the 422 path. The fixture task
// declares one optional param "label"; sending an unknown key should be
// rejected with per-field detail in the response body.
func TestAPI_TestTask_SchemaMismatch(t *testing.T) {
	srv, reg := newTestServer(t)
	registerTaskWithTest(t, reg, "schema-task", "", passingTest)

	body := `{"params": {"unknown_key": "boom"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/schema-task/test", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Error  string            `json:"error"`
		Fields []task.ParamError `json:"fields"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Fields) != 1 || resp.Fields[0].Field != "unknown_key" {
		t.Errorf("expected single field error on unknown_key, got %+v", resp.Fields)
	}
}

// TestAPI_TestTask_Timeout covers the 408 path. The fixture's test file
// hangs forever; the POST sets timeout_s=1 and we assert HTTP 408 with
// status="timeout" in the body.
func TestAPI_TestTask_Timeout(t *testing.T) {
	srv, reg := newTestServer(t)
	registerTaskWithTest(t, reg, "hang-task", "", hangingTest)

	body := `{"timeout_s": 1}`
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/hang-task/test", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	start := time.Now()
	srv.Handler().ServeHTTP(w, req)
	elapsed := time.Since(start)

	if w.Code != http.StatusRequestTimeout {
		t.Fatalf("expected 408, got %d in %v: %s", w.Code, elapsed, w.Body.String())
	}
	// Generous upper bound: deno startup + 1s timeout + cleanup.
	if elapsed > 30*time.Second {
		t.Errorf("timeout took too long: %v", elapsed)
	}
	var resp testTaskResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "timeout" {
		t.Errorf("status: got %q, want timeout", resp.Status)
	}
}

// TestAPI_TestTask_SharedHelperInvariant pins the refactor-fence: both the
// REST handler (apiTestTask) and the CLI/IPC handler (handleTaskTest in
// pkg/ipc/control.go) must route through tasktest.RunByID. We can't import
// pkg/ipc here without a cycle, so we exercise RunByID directly with the
// same registry+id and assert the structural Result equals what the REST
// path produces.
//
// Concretely: take the underlying tasktest.Result from a direct RunByID
// call and from a captured-from-handler buildTestTaskResponse. The Stdout,
// Passed/Failed counts, ExitCode, and TestFile must agree (DurationMs and
// RunID are intrinsically per-call so we don't compare those).
func TestAPI_TestTask_SharedHelperInvariant(t *testing.T) {
	srv, reg := newTestServer(t)
	registerTaskWithTest(t, reg, "invariant-task", "", passingTest)

	// 1) Direct RunByID call — what every other call-site (CLI, future
	// SDK plumbing) goes through.
	directRes, _, directErr := tasktest.RunByID(context.Background(), reg, "invariant-task", nil, 0)
	if directErr != nil {
		t.Fatalf("direct RunByID failed: %v", directErr)
	}

	// 2) Same task via the REST handler.
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/invariant-task/test", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("REST call: expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var restResp testTaskResponse
	if err := json.NewDecoder(w.Body).Decode(&restResp); err != nil {
		t.Fatalf("decode REST resp: %v", err)
	}

	// Compare the load-bearing fields. The runner is deterministic for a
	// passing fixture, so counts and exit codes must match exactly. Stdout
	// strings carry timing info ("(1ms)"), so compare that the runner's
	// summary line ("ok | 1 passed") appears in both — pinning structural
	// equality without making the test flaky on timing jitter.
	if restResp.Passed != directRes.Passed {
		t.Errorf("passed mismatch: REST=%d direct=%d", restResp.Passed, directRes.Passed)
	}
	if restResp.Failed != directRes.Failed {
		t.Errorf("failed mismatch: REST=%d direct=%d", restResp.Failed, directRes.Failed)
	}
	if restResp.ExitCode != directRes.ExitCode {
		t.Errorf("exit_code mismatch: REST=%d direct=%d", restResp.ExitCode, directRes.ExitCode)
	}
	if restResp.TestFile != directRes.TestFile {
		t.Errorf("test_file mismatch: REST=%q direct=%q", restResp.TestFile, directRes.TestFile)
	}
	if !strings.Contains(restResp.Stdout, "1 passed") || !strings.Contains(directRes.Output, "1 passed") {
		t.Errorf("expected '1 passed' in both outputs; rest=%q direct=%q", restResp.Stdout, directRes.Output)
	}
}
