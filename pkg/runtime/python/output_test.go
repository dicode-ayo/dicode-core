//go:build !windows

package python

import (
	"context"
	"testing"
	"time"

	pkgruntime "github.com/dicode/dicode/pkg/runtime"
)

// TestExecute_ReturnValuePersisted is the regression test for #680: a Python
// task's `result` assignment must reach RunResult.ReturnValue so
// dicode.run_task (pkg/ipc/server.go) sees it instead of null.
func TestExecute_ReturnValuePersisted(t *testing.T) {
	if testing.Short() {
		t.Skip("requires uv subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	reg, ex := newSuspendExecutor(t)

	spec := writePythonTask(t, "examples/return-value", `
async def main():
    return {"count": 42}
`)
	if err := reg.Register(spec); err != nil {
		t.Fatal(err)
	}
	runID, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	res, err := ex.Execute(ctx, spec, pkgruntime.RunOptions{RunID: runID})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Error != nil {
		dumpRunLogs(t, reg, runID)
		t.Fatalf("run failed: %v", res.Error)
	}

	if res.ReturnValue == nil {
		t.Fatal("ReturnValue is nil — #680 regression: Python runs never populate ReturnValue")
	}
	m, ok := res.ReturnValue.(map[string]any)
	if !ok {
		t.Fatalf("ReturnValue is %T, want map[string]any: %+v", res.ReturnValue, res.ReturnValue)
	}
	if count, _ := m["count"].(float64); count != 42 {
		t.Fatalf("ReturnValue[\"count\"] = %v, want 42", m["count"])
	}
}

// TestExecute_OutputHTMLPersisted is the regression test for #680: a Python
// task calling output.html(...) must populate RunResult.OutputContentType /
// OutputContent so pkg/trigger/webhook.go can serve it as the webhook
// response body, matching the Deno runtime and the documented parity table
// (docs/python-runtime.md).
func TestExecute_OutputHTMLPersisted(t *testing.T) {
	if testing.Short() {
		t.Skip("requires uv subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	reg, ex := newSuspendExecutor(t)

	spec := writePythonTask(t, "examples/output-html", `
async def main():
    output.html("<h1>hi</h1>")
`)
	if err := reg.Register(spec); err != nil {
		t.Fatal(err)
	}
	runID, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	res, err := ex.Execute(ctx, spec, pkgruntime.RunOptions{RunID: runID})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Error != nil {
		dumpRunLogs(t, reg, runID)
		t.Fatalf("run failed: %v", res.Error)
	}

	if res.OutputContentType != "text/html" {
		t.Fatalf("OutputContentType = %q, want %q — #680 regression: Python runs never populate structured output", res.OutputContentType, "text/html")
	}
	if res.OutputContent != "<h1>hi</h1>" {
		t.Fatalf("OutputContent = %q, want %q", res.OutputContent, "<h1>hi</h1>")
	}
}

// TestExecute_OutputJSONPersisted holds the Python SDK to the Deno shim's
// output surface: output.json must reach RunResult as an application/json
// body, or a task written against the documented parity table (see
// docs/python-runtime.md) silently answers its webhook caller with nothing.
func TestExecute_OutputJSONPersisted(t *testing.T) {
	if testing.Short() {
		t.Skip("requires uv subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	reg, ex := newSuspendExecutor(t)

	spec := writePythonTask(t, "examples/output-json", `
async def main():
    output.json({"ok": False, "error": "boom"})
`)
	if err := reg.Register(spec); err != nil {
		t.Fatal(err)
	}
	runID, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	res, err := ex.Execute(ctx, spec, pkgruntime.RunOptions{RunID: runID})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Error != nil {
		dumpRunLogs(t, reg, runID)
		t.Fatalf("run failed: %v", res.Error)
	}

	if res.OutputContentType != "application/json" {
		t.Fatalf("OutputContentType = %q, want %q", res.OutputContentType, "application/json")
	}
	if res.OutputContent != `{"ok": false, "error": "boom"}` {
		t.Fatalf("OutputContent = %q", res.OutputContent)
	}
}
