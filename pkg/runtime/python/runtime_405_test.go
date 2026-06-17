//go:build !windows

package python

import (
	"context"
	"net/http"
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
// httpx GET to httpbin.org) must run to completion. It provisions uv the way
// the runtime itself does — uv is not assumed to be on PATH — and reaches the
// public internet, so it skips when uv cannot be provisioned. It guards the
// async-main invocation (the kwargs bug that broke the run) and confirms the
// PEP 578 net guard admits an allowlisted httpx call.
func TestExecute_HelloPythonSucceeds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network/uv integration test in -short mode")
	}
	uv, err := uvpkg.EnsureUv("")
	if err != nil {
		t.Skipf("uv provisioning failed (offline?): %v", err)
	}

	// Pre-flight: skip if httpbin.org is not returning success (rate-limit, block, etc.).
	pf, pfErr := http.Get("https://httpbin.org/get") //nolint:noctx
	if pfErr != nil {
		t.Skipf("httpbin.org unreachable: %v", pfErr)
	}
	pf.Body.Close()
	if pf.StatusCode >= 300 {
		t.Skipf("httpbin.org returned %d, skipping network integration test", pf.StatusCode)
	}

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
		},
		Permissions: task.Permissions{Net: []string{"httpbin.org"}},
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
