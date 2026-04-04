package registry

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/task"
)

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return New(d)
}

func makeSpec(id string) *task.Spec {
	return &task.Spec{
		ID:      id,
		Name:    id,
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true},
		Timeout: 30 * time.Second,
	}
}

func TestRegistry_RegisterGetAll(t *testing.T) {
	r := newTestRegistry(t)

	_ = r.Register(makeSpec("task-a"))
	_ = r.Register(makeSpec("task-b"))

	if s, ok := r.Get("task-a"); !ok || s.ID != "task-a" {
		t.Errorf("Get task-a: ok=%v, spec=%v", ok, s)
	}
	if _, ok := r.Get("missing"); ok {
		t.Error("Get missing should return false")
	}

	all := r.All()
	if len(all) != 2 {
		t.Errorf("All: expected 2, got %d", len(all))
	}
}

func TestRegistry_Unregister(t *testing.T) {
	r := newTestRegistry(t)
	_ = r.Register(makeSpec("task-x"))
	r.Unregister("task-x")

	if _, ok := r.Get("task-x"); ok {
		t.Error("task-x should be unregistered")
	}
}

func TestRegistry_Register_Upsert(t *testing.T) {
	r := newTestRegistry(t)
	s := makeSpec("task-u")
	s.Name = "original"
	_ = r.Register(s)

	s2 := makeSpec("task-u")
	s2.Name = "updated"
	_ = r.Register(s2)

	got, _ := r.Get("task-u")
	if got.Name != "updated" {
		t.Errorf("expected updated, got %s", got.Name)
	}
}

func TestRegistry_RunLifecycle(t *testing.T) {
	r := newTestRegistry(t)
	_ = r.Register(makeSpec("task-r"))
	ctx := context.Background()

	runID, err := r.StartRun(ctx, "task-r", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if runID == "" {
		t.Fatal("empty run ID")
	}

	run, err := r.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != StatusRunning {
		t.Errorf("expected running, got %s", run.Status)
	}

	if err := r.FinishRun(ctx, runID, StatusSuccess); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	run, _ = r.GetRun(ctx, runID)
	if run.Status != StatusSuccess {
		t.Errorf("expected success, got %s", run.Status)
	}
	if run.FinishedAt == nil {
		t.Error("FinishedAt should be set")
	}
}

func TestRegistry_AppendLog_GetRunLogs(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	runID, _ := r.StartRun(ctx, "task-l", "")

	_ = r.AppendLog(ctx, runID, "info", "starting")
	_ = r.AppendLog(ctx, runID, "warn", "something odd")
	_ = r.AppendLog(ctx, runID, "error", "boom")

	logs, err := r.GetRunLogs(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunLogs: %v", err)
	}
	if len(logs) != 3 {
		t.Fatalf("expected 3 log entries, got %d", len(logs))
	}
	if logs[0].Message != "starting" || logs[2].Level != "error" {
		t.Errorf("unexpected log content: %+v", logs)
	}
}

func TestRegistry_ListRuns(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		id, _ := r.StartRun(ctx, "task-m", "")
		_ = r.FinishRun(ctx, id, StatusSuccess)
	}

	runs, err := r.ListRuns(ctx, "task-m", 3)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 3 {
		t.Errorf("expected 3, got %d", len(runs))
	}
}

func TestRegistry_ParentRunID(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	parentID, _ := r.StartRun(ctx, "parent-task", "")
	childID, _ := r.StartRun(ctx, "child-task", parentID)

	child, err := r.GetRun(ctx, childID)
	if err != nil {
		t.Fatalf("GetRun child: %v", err)
	}
	if child.ParentRunID != parentID {
		t.Errorf("expected parentRunID=%s, got %s", parentID, child.ParentRunID)
	}
}

// ── BulkAppendLogs tests ──────────────────────────────────────────────────────

func TestRegistry_BulkAppendLogs_Empty(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	// Should be a no-op and not return an error.
	if err := r.BulkAppendLogs(ctx, nil); err != nil {
		t.Fatalf("BulkAppendLogs(nil): %v", err)
	}
	if err := r.BulkAppendLogs(ctx, []PendingLogEntry{}); err != nil {
		t.Fatalf("BulkAppendLogs([]): %v", err)
	}
}

func TestRegistry_BulkAppendLogs_Single(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	runID, _ := r.StartRun(ctx, "t", "")

	entries := []PendingLogEntry{
		{RunID: runID, Level: "info", Message: "hello", TsMs: 1000},
	}
	if err := r.BulkAppendLogs(ctx, entries); err != nil {
		t.Fatalf("BulkAppendLogs: %v", err)
	}
	logs, err := r.GetRunLogs(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunLogs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("expected 1 log, got %d", len(logs))
	}
	if logs[0].Message != "hello" || logs[0].Level != "info" {
		t.Errorf("unexpected entry: %+v", logs[0])
	}
}

func TestRegistry_BulkAppendLogs_Multiple(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	runID, _ := r.StartRun(ctx, "t", "")

	const n = 30
	entries := make([]PendingLogEntry, n)
	baseTs := int64(2000)
	for i := range entries {
		entries[i] = PendingLogEntry{
			RunID:   runID,
			Level:   "info",
			Message: fmt.Sprintf("msg-%02d", i),
			TsMs:    baseTs + int64(i),
		}
	}
	if err := r.BulkAppendLogs(ctx, entries); err != nil {
		t.Fatalf("BulkAppendLogs: %v", err)
	}
	logs, err := r.GetRunLogs(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunLogs: %v", err)
	}
	if len(logs) != n {
		t.Fatalf("expected %d logs, got %d", n, len(logs))
	}
	for i, lg := range logs {
		want := fmt.Sprintf("msg-%02d", i)
		if lg.Message != want {
			t.Errorf("log[%d]: got %q, want %q", i, lg.Message, want)
		}
	}
}

func TestRegistry_BulkAppendLogs_HookFired(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	runID, _ := r.StartRun(ctx, "t", "")

	var mu sync.Mutex
	var hooked []string
	r.SetLogHook(func(_ string, _ string, msg string, _ int64) {
		mu.Lock()
		hooked = append(hooked, msg)
		mu.Unlock()
	})

	entries := []PendingLogEntry{
		{RunID: runID, Level: "info", Message: "a", TsMs: 1},
		{RunID: runID, Level: "warn", Message: "b", TsMs: 2},
	}
	if err := r.BulkAppendLogs(ctx, entries); err != nil {
		t.Fatalf("BulkAppendLogs: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hooked) != 2 || hooked[0] != "a" || hooked[1] != "b" {
		t.Errorf("hook got %v, want [a b]", hooked)
	}
}
