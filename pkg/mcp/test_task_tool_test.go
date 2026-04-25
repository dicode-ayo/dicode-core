package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
)

// Tests for the test_task MCP tool added in PR #160. Drive the tool through
// the public HTTP transport (POST /) so the entire mcp-go pipeline is
// exercised end-to-end — listing, argument validation, and error envelopes.

func newMCPServerForTest(t *testing.T) (*Server, *registry.Registry) {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	reg := registry.New(d)
	return New(reg, nil), reg
}

// callMCP POSTs a JSON-RPC request to the server's HTTP handler and returns
// the decoded response. Tests can inspect raw fields without re-implementing
// the wire decode.
func callMCP(t *testing.T, s *Server, method string, params any) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v\nbody = %s", err, w.Body.String())
	}
	return resp
}

// callTool wraps callMCP for tools/call requests and returns the inner result
// or the JSON-RPC error envelope, whichever is present.
func callTool(t *testing.T, s *Server, name string, args map[string]any) (result map[string]any, errMsg string) {
	t.Helper()
	resp := callMCP(t, s, "tools/call", map[string]any{"name": name, "arguments": args})
	if errVal, ok := resp["error"].(map[string]any); ok {
		msg, _ := errVal["message"].(string)
		return nil, msg
	}
	res, _ := resp["result"].(map[string]any)
	return res, ""
}

// firstText returns the text content of the first text block in a tools/call
// result, or "" if the structure is unexpected.
func firstText(result map[string]any) string {
	content, _ := result["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	return text
}

func resultIsError(result map[string]any) bool {
	v, _ := result["isError"].(bool)
	return v
}

// TestToolsList_ExposesTestTask verifies the tool is advertised in the
// tools/list response so MCP clients can discover it.
func TestToolsList_ExposesTestTask(t *testing.T) {
	s, _ := newMCPServerForTest(t)
	resp := callMCP(t, s, "tools/list", nil)

	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("result missing or wrong type: %+v", resp)
	}
	tools, _ := result["tools"].([]any)
	var testTask map[string]any
	names := make([]string, 0, len(tools))
	for _, raw := range tools {
		tt, _ := raw.(map[string]any)
		name, _ := tt["name"].(string)
		names = append(names, name)
		if name == "test_task" {
			testTask = tt
		}
	}
	if testTask == nil {
		t.Fatalf("test_task not listed in tools/list (got: %v)", names)
	}
	desc, _ := testTask["description"].(string)
	if !strings.Contains(desc, "task.test") {
		t.Errorf("description should mention task.test: %q", desc)
	}
	// Schema must require `id`.
	schema, _ := testTask["inputSchema"].(map[string]any)
	required, _ := schema["required"].([]any)
	foundID := false
	for _, r := range required {
		if r == "id" {
			foundID = true
		}
	}
	if !foundID {
		t.Errorf("test_task schema must require 'id': %+v", schema)
	}
}

// TestToolTestTask_MissingID returns a tool error when arguments omit id.
func TestToolTestTask_MissingID(t *testing.T) {
	s, _ := newMCPServerForTest(t)
	result, errMsg := callTool(t, s, "test_task", map[string]any{})
	// mcp-go's NewTool with Required() returns the missing-arg error via the
	// CallToolRequest.RequireString helper as a tool error (isError=true with
	// content), not via the JSON-RPC error envelope.
	if errMsg != "" {
		// Acceptable too — older mcp-go versions used the error envelope.
		if !strings.Contains(strings.ToLower(errMsg), "id") {
			t.Errorf("error msg should mention id: %q", errMsg)
		}
		return
	}
	if !resultIsError(result) {
		t.Fatalf("expected tool error for missing id, got result=%v", result)
	}
	text := strings.ToLower(firstText(result))
	if !strings.Contains(text, "id") {
		t.Errorf("error text should mention id: %q", text)
	}
}

// TestToolTestTask_UnknownTask returns an error when the id doesn't
// resolve to a registered task.
func TestToolTestTask_UnknownTask(t *testing.T) {
	s, _ := newMCPServerForTest(t)
	result, errMsg := callTool(t, s, "test_task", map[string]any{"id": "does/not/exist"})
	if errMsg != "" {
		if !strings.Contains(errMsg, "not found") {
			t.Errorf("error msg = %q, want to contain 'not found'", errMsg)
		}
		return
	}
	if !resultIsError(result) {
		t.Fatalf("expected tool error for unknown task, got result=%v", result)
	}
	text := firstText(result)
	if !strings.Contains(text, "not found") {
		t.Errorf("error text = %q, want to contain 'not found'", text)
	}
}

// TestToolTestTask_UnsupportedRuntime_StructuredSummary verifies the
// summary block is emitted even when the underlying runner refuses the
// runtime. MCP clients parse `--- summary ---` to pull structured counts.
func TestToolTestTask_UnsupportedRuntime_StructuredSummary(t *testing.T) {
	s, reg := newMCPServerForTest(t)

	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "task.yaml"), []byte("name: x\ntrigger:\n  manual: true\nruntime: docker\n"), 0644)
	_ = os.WriteFile(filepath.Join(dir, "task.test.ts"), []byte(""), 0644)
	spec := &task.Spec{
		ID: "x/docker", Name: "x", Runtime: task.RuntimeDocker,
		Trigger: task.TriggerConfig{Manual: true}, Timeout: 5 * time.Second, TaskDir: dir,
	}
	_ = reg.Register(spec)

	result, errMsg := callTool(t, s, "test_task", map[string]any{"id": "x/docker"})
	if errMsg != "" {
		t.Fatalf("tool call errored instead of returning partial result: %v", errMsg)
	}
	text := firstText(result)
	if !strings.Contains(text, "--- summary ---") {
		t.Errorf("response missing summary block: %q", text)
	}
	if !strings.Contains(text, "not yet supported") && !strings.Contains(text, "#159") {
		t.Errorf("unsupported-runtime error not surfaced: %q", text)
	}
}
