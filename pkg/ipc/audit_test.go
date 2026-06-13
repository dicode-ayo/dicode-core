package ipc

import (
	"context"
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/audit"
	"github.com/dicode/dicode/pkg/task"
)

// auditEvents queries all audit rows recorded against the test env's DB.
func auditEvents(t *testing.T, e *testEnv, f audit.Filter) []audit.Event {
	t.Helper()
	events, err := audit.NewStore(e.db).Query(context.Background(), f)
	if err != nil {
		t.Fatalf("audit query: %v", err)
	}
	return events
}

func TestAudit_RunTask_DeniedNoCap(t *testing.T) {
	e := newTestEnv(t)
	conn, _ := e.start(t, nil, nil) // no spec → no tasks.trigger cap

	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.run_task", "taskID": "some-task"})
	if resp := recvMsg(t, conn); resp["error"] == nil {
		t.Fatal("expected permission denied")
	}

	events := auditEvents(t, e, audit.Filter{EventType: audit.EventTaskCalled})
	if len(events) != 1 {
		t.Fatalf("got %d task_called events, want 1", len(events))
	}
	ev := events[0]
	if ev.Allowed {
		t.Error("denied call must record allowed=false")
	}
	if ev.Reason != "permission denied (tasks.trigger)" {
		t.Errorf("reason: %q", ev.Reason)
	}
	if ev.ActorKind != "task" || ev.ActorID != "test-task" {
		t.Errorf("actor: %s/%s", ev.ActorKind, ev.ActorID)
	}
	if ev.TargetID != "some-task" {
		t.Errorf("target: %q", ev.TargetID)
	}
}

func TestAudit_RunTask_DeniedNotAllowedAndParamsSanitized(t *testing.T) {
	e := newTestEnv(t)
	spec := specWithDicode("caller", &task.DicodePermissions{Tasks: []string{"permitted-task"}})
	conn, _ := e.startWithSpec(t, nil, nil, spec, &mockEngine{})

	sendMsg(t, conn, map[string]any{
		"id": "1", "method": "dicode.run_task", "taskID": "forbidden-task",
		"params": map[string]string{"api_key": "sk-live-supersecret", "channel": "#ops"},
	})
	if resp := recvMsg(t, conn); resp["error"] == nil {
		t.Fatal("expected security error for unlisted task")
	}

	events := auditEvents(t, e, audit.Filter{TaskID: "forbidden-task"})
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.Allowed || ev.Reason != "not in security.allowed_tasks" {
		t.Errorf("allowed=%v reason=%q", ev.Allowed, ev.Reason)
	}
	if strings.Contains(ev.Params, "sk-live-supersecret") {
		t.Errorf("audit params leaked secret value: %s", ev.Params)
	}
	if !strings.Contains(ev.Params, audit.Redacted) {
		t.Errorf("expected redaction placeholder in params: %s", ev.Params)
	}
	if !strings.Contains(ev.Params, "#ops") {
		t.Errorf("non-sensitive param missing: %s", ev.Params)
	}
}

func TestAudit_RunTask_Allowed(t *testing.T) {
	e := newTestEnv(t)
	eng := &mockEngine{runID: "run-abc", result: RunResult{RunID: "run-abc", Status: "success"}}
	spec := specWithDicode("caller", &task.DicodePermissions{Tasks: []string{"target-task"}})
	conn, _ := e.startWithSpec(t, nil, nil, spec, eng)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.run_task", "taskID": "target-task"})
	if resp := recvMsg(t, conn); resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}

	events := auditEvents(t, e, audit.Filter{TaskID: "target-task"})
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if !ev.Allowed || ev.Reason != "" {
		t.Errorf("allowed=%v reason=%q, want allowed with empty reason", ev.Allowed, ev.Reason)
	}
	if ev.RunID != "run-abc" {
		t.Errorf("run_id: got %q, want the fired run id", ev.RunID)
	}
	if ev.EventType != audit.EventTaskCalled {
		t.Errorf("event_type: %q", ev.EventType)
	}
}

func TestAudit_RunTask_MCPContextEmitsMCPCalled(t *testing.T) {
	e := newTestEnv(t)
	spec := specWithDicode("caller", &task.DicodePermissions{Tasks: []string{"*"}})
	conn, _ := e.startWithSpec(t, nil, nil, spec, &mockEngine{})

	// MCP-context call against a task that is not registered → denied with
	// "task not found", recorded as mcp_called (the tools/call boundary).
	sendMsg(t, conn, map[string]any{
		"id": "1", "method": "dicode.run_task", "taskID": "ghost-task", "mcpContext": true,
	})
	if resp := recvMsg(t, conn); resp["error"] == nil {
		t.Fatal("expected error for unknown MCP-exposed task")
	}

	events := auditEvents(t, e, audit.Filter{EventType: audit.EventMCPCalled})
	if len(events) != 1 {
		t.Fatalf("got %d mcp_called events, want 1", len(events))
	}
	if events[0].Allowed || events[0].Reason != "task not found" {
		t.Errorf("allowed=%v reason=%q", events[0].Allowed, events[0].Reason)
	}
}

func TestAudit_MCPCall_Denied(t *testing.T) {
	e := newTestEnv(t)
	conn, _ := e.start(t, nil, nil) // no spec → no mcp.call cap

	sendMsg(t, conn, map[string]any{
		"id": "1", "method": "mcp.call", "mcpName": "github", "tool": "list_issues",
		"args": map[string]any{"token": "ghp_secret", "repo": "dicode-core"},
	})
	if resp := recvMsg(t, conn); resp["error"] == nil {
		t.Fatal("expected permission denied")
	}

	events := auditEvents(t, e, audit.Filter{EventType: audit.EventMCPCalled})
	if len(events) != 1 {
		t.Fatalf("got %d mcp_called events, want 1", len(events))
	}
	ev := events[0]
	if ev.Allowed {
		t.Error("denied mcp.call must record allowed=false")
	}
	if ev.TargetKind != "mcp" || ev.TargetID != "github/list_issues" {
		t.Errorf("target: %s/%s", ev.TargetKind, ev.TargetID)
	}
	if strings.Contains(ev.Params, "ghp_secret") {
		t.Errorf("audit params leaked secret arg: %s", ev.Params)
	}
	if !strings.Contains(ev.Params, "dicode-core") {
		t.Errorf("non-sensitive arg missing: %s", ev.Params)
	}
}
