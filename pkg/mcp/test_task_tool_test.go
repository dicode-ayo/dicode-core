package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
)

// Tests for the test_task MCP tool added in PR #160. The tool surfaces
// pkg/tasktest.Run to any MCP-capable client; these tests verify the
// listing, argument validation, and error paths are wired correctly.
// The end-to-end happy path (spawning Deno) is covered by the parallel
// test in pkg/ipc/control_task_test_test.go to avoid double-running
// the slow test across packages.

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

// TestToolsList_ExposesTestTask verifies the tool is advertised in the
// tools/list response so MCP clients can discover it.
func TestToolsList_ExposesTestTask(t *testing.T) {
	s, _ := newMCPServerForTest(t)
	resp := s.methodToolsList(rpcRequest{ID: 1})

	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result type = %T, want map[string]any", resp.Result)
	}
	tools, _ := result["tools"].([]tool)
	var found *tool
	for i := range tools {
		if tools[i].Name == "test_task" {
			found = &tools[i]
			break
		}
	}
	if found == nil {
		names := make([]string, 0, len(tools))
		for _, t := range tools {
			names = append(names, t.Name)
		}
		t.Fatalf("test_task not listed in tools/list (got: %v)", names)
	}
	if !strings.Contains(found.Description, "task.test") {
		t.Errorf("description should mention task.test: %q", found.Description)
	}
	// Schema must require `id`. jsonSchema() stores the variadic
	// []string directly, so assert the concrete slice type.
	required, ok := found.InputSchema["required"].([]string)
	if !ok {
		t.Fatalf("required must be []string, got %T: %+v", found.InputSchema["required"], found.InputSchema)
	}
	foundID := false
	for _, r := range required {
		if r == "id" {
			foundID = true
		}
	}
	if !foundID {
		t.Errorf("test_task schema must require 'id': %+v", found.InputSchema)
	}
}

// TestToolTestTask_MissingID returns a JSON-RPC error when arguments omit id.
func TestToolTestTask_MissingID(t *testing.T) {
	s, _ := newMCPServerForTest(t)
	resp := s.toolTestTask(context.Background(), 1, json.RawMessage(`{}`))
	if resp.Error == nil {
		t.Fatalf("expected error for missing id, got result=%v", resp.Result)
	}
	if !strings.Contains(resp.Error.Message, "id is required") {
		t.Errorf("error msg = %q, want 'id is required'", resp.Error.Message)
	}
}

// TestToolTestTask_MalformedArgs returns -32602 on invalid JSON.
func TestToolTestTask_MalformedArgs(t *testing.T) {
	s, _ := newMCPServerForTest(t)
	resp := s.toolTestTask(context.Background(), 1, json.RawMessage(`not json`))
	if resp.Error == nil {
		t.Fatal("expected error for malformed args")
	}
	if resp.Error.Code != -32602 {
		t.Errorf("code = %d, want -32602 (invalid params)", resp.Error.Code)
	}
}

// TestToolTestTask_UnknownTask returns an error when the id doesn't
// resolve to a registered task.
func TestToolTestTask_UnknownTask(t *testing.T) {
	s, _ := newMCPServerForTest(t)
	resp := s.toolTestTask(context.Background(), 1, json.RawMessage(`{"id":"does/not/exist"}`))
	if resp.Error == nil {
		t.Fatal("expected error for unknown task")
	}
	if !strings.Contains(resp.Error.Message, "not found") {
		t.Errorf("error = %q, want to contain 'not found'", resp.Error.Message)
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

	resp := s.toolTestTask(context.Background(), 1, json.RawMessage(`{"id":"x/docker"}`))
	if resp.Error != nil {
		t.Fatalf("tool call errored instead of returning partial result: %v", resp.Error)
	}

	// Response shape: content[0].text contains "<output>\n\n--- summary ---\n<json>"
	result, _ := resp.Result.(map[string]any)
	content, _ := result["content"].([]map[string]any)
	if len(content) == 0 {
		t.Fatalf("no content in response: %+v", resp.Result)
	}
	text, _ := content[0]["text"].(string)
	if !strings.Contains(text, "--- summary ---") {
		t.Errorf("response missing summary block: %q", text)
	}
	// The error field should mention the tracked issue for parity.
	if !strings.Contains(text, "not yet supported") && !strings.Contains(text, "#159") {
		t.Errorf("unsupported-runtime error not surfaced: %q", text)
	}
}
