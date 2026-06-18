//go:build !windows

package python

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
	uvpkg "github.com/dicode/dicode/pkg/uv"
	"go.uber.org/zap"
)

func newTestRuntime(t *testing.T) (*Runtime, *registry.Registry) {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)
	rt, err := New(reg, nil, d, zap.NewNop())
	if err != nil {
		t.Fatalf("python.New: %v", err)
	}
	return rt, reg
}

// TestExecute_FailedRunCapturesDiagnostics asserts the issue #405 invariant:
// a Python run that fails before (or without) emitting subprocess stderr must
// still leave its error in the run log. The "script not found" early return
// exercises a failure path that never starts uv, so the only way the error
// reaches the log is the runtime's diagnostic safety net.
func TestExecute_FailedRunCapturesDiagnostics(t *testing.T) {
	rt, reg := newTestRuntime(t)
	ex := rt.NewExecutor("/nonexistent/uv")

	spec := &task.Spec{
		ID:      "examples/no-script",
		Name:    "No Script",
		Runtime: task.Runtime("python"),
		TaskDir: t.TempDir(), // empty dir → ScriptPath() == "" → early failure
		Trigger: task.TriggerConfig{Manual: true},
	}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("register: %v", err)
	}

	ctx := context.Background()
	runID, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	res, err := ex.Execute(ctx, spec, pkgruntime.RunOptions{RunID: runID})
	if err != nil {
		t.Fatalf("Execute returned a transport error: %v", err)
	}
	if res.Error == nil {
		t.Fatal("expected a run error, got nil")
	}

	logs, err := reg.GetRunLogs(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunLogs: %v", err)
	}
	if len(logs) == 0 {
		t.Fatal("failed run captured zero log lines — #405 regression")
	}
	var found bool
	for _, l := range logs {
		if strings.Contains(l.Message, res.Error.Error()) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("run log does not contain the error %q; got %d lines", res.Error.Error(), len(logs))
	}
}

// TestExecute_HelloPythonSucceeds is the end-to-end regression for #405: the
// hello-python example (async def main(), PEP 723 httpx dep, an allowlisted
// httpx GET) must run to completion. It provisions uv the way the runtime
// itself does and skips when uv cannot be provisioned.
//
// The test spins up a local httptest.Server instead of reaching httpbin.org,
// eliminating the external dependency that caused CI flakiness (#422). The
// local server still validates the PEP 578 net guard: IP literals (127.0.0.1)
// are allowed by the guard regardless of the allowlist, so the test uses
// permissions.net: ["127.0.0.1"] to engage allowlist mode rather than
// unrestricted mode while still permitting the loopback connection.
func TestExecute_HelloPythonSucceeds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping uv integration test in -short mode")
	}
	uv, err := uvpkg.EnsureUv("")
	if err != nil {
		t.Skipf("uv provisioning failed (offline?): %v", err)
	}

	// Local httpbin-compatible server — no external network required.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"origin": "127.0.0.1",
			"args":   r.URL.Query(),
			"url":    r.URL.String(),
		})
	}))
	t.Cleanup(srv.Close)

	rt, reg := newTestRuntime(t)
	ex := rt.NewExecutor(uv)

	spec := &task.Spec{
		ID:      "examples/hello-python",
		Name:    "Hello Python",
		Runtime: task.Runtime("python"),
		TaskDir: "../../../tasks/examples/hello-python",
		Trigger: task.TriggerConfig{Manual: true},
		Timeout: 120 * time.Second,
		Params: []task.Param{
			{Name: "name", Default: "World"},
			{Name: "count", Default: "1"},
			{Name: "httpbin_url", Default: "https://httpbin.org/get"},
		},
		// 127.0.0.1 is an IP literal — the PEP 578 guard admits it in
		// allowlist mode without needing an explicit allowlist entry. Using
		// a non-empty net list here engages allowlist mode rather than deny.
		Permissions: task.Permissions{Net: []string{"127.0.0.1"}},
	}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("register: %v", err)
	}

	ctx := context.Background()
	runID, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	res, err := ex.Execute(ctx, spec, pkgruntime.RunOptions{
		RunID:  runID,
		Params: map[string]string{"httpbin_url": srv.URL + "/get"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	logs, _ := reg.GetRunLogs(ctx, runID)
	if res.Error != nil {
		t.Fatalf("hello-python failed: %v\nlogs:\n%s", res.Error, joinLogs(logs))
	}
	greeted := false
	for _, l := range logs {
		if strings.Contains(l.Message, "Hello, World!") {
			greeted = true
			break
		}
	}
	if !greeted {
		t.Fatalf("expected greeting log line; got:\n%s", joinLogs(logs))
	}
}

func joinLogs(logs []*registry.LogEntry) string {
	var b strings.Builder
	for _, l := range logs {
		b.WriteString("  [" + l.Level + "] " + l.Message + "\n")
	}
	return b.String()
}
