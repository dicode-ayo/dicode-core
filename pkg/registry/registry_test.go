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
	t.Parallel()
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
	t.Parallel()
	r := newTestRegistry(t)
	_ = r.Register(makeSpec("task-x"))
	r.Unregister("task-x")

	if _, ok := r.Get("task-x"); ok {
		t.Error("task-x should be unregistered")
	}
}

func TestRegistry_Register_Upsert(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// ── #116 run grouping ─────────────────────────────────────────────────────────

func TestRegistry_SetRunGroup(t *testing.T) {
	t.Parallel()
	r := newTestRegistry(t)
	ctx := context.Background()

	runID, _ := r.StartRun(ctx, "task-g", "")
	if err := r.SetRunGroup(ctx, runID, "chat-42"); err != nil {
		t.Fatalf("SetRunGroup: %v", err)
	}
	got, err := r.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Group != "chat-42" {
		t.Errorf("Group = %q, want %q", got.Group, "chat-42")
	}

	// Last write wins.
	if err := r.SetRunGroup(ctx, runID, "chat-99"); err != nil {
		t.Fatalf("SetRunGroup overwrite: %v", err)
	}
	got, _ = r.GetRun(ctx, runID)
	if got.Group != "chat-99" {
		t.Errorf("Group after overwrite = %q, want chat-99", got.Group)
	}
}

func TestRegistry_ListChildren(t *testing.T) {
	t.Parallel()
	r := newTestRegistry(t)
	ctx := context.Background()

	parentID, _ := r.StartRun(ctx, "parent", "")
	c1, _ := r.StartRun(ctx, "child-task", parentID)
	c2, _ := r.StartRun(ctx, "child-task", parentID)
	// An unrelated run that must NOT appear.
	_, _ = r.StartRun(ctx, "other", "")

	kids, err := r.ListChildren(ctx, parentID, 10)
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	if len(kids) != 2 {
		t.Fatalf("len(kids) = %d, want 2", len(kids))
	}
	got := map[string]bool{kids[0].ID: true, kids[1].ID: true}
	if !got[c1] || !got[c2] {
		t.Errorf("missing child IDs in result: %+v", got)
	}
}

func TestRegistry_ListByGroup(t *testing.T) {
	t.Parallel()
	r := newTestRegistry(t)
	ctx := context.Background()

	a1, _ := r.StartRun(ctx, "task-a", "")
	a2, _ := r.StartRun(ctx, "task-a", "")
	b1, _ := r.StartRun(ctx, "task-b", "")
	_ = a2
	// task-a runs share group "g1"; task-b uses the same label but different
	// task, so it must be excluded.
	for _, id := range []string{a1, a2} {
		if err := r.SetRunGroup(ctx, id, "g1"); err != nil {
			t.Fatalf("SetRunGroup: %v", err)
		}
	}
	if err := r.SetRunGroup(ctx, b1, "g1"); err != nil {
		t.Fatalf("SetRunGroup b1: %v", err)
	}

	siblings, err := r.ListByGroup(ctx, "task-a", "g1", 10)
	if err != nil {
		t.Fatalf("ListByGroup: %v", err)
	}
	if len(siblings) != 2 {
		t.Fatalf("len(siblings) = %d, want 2", len(siblings))
	}
	for _, s := range siblings {
		if s.TaskID != "task-a" {
			t.Errorf("got run from task %q in task-a group", s.TaskID)
		}
		if s.Group != "g1" {
			t.Errorf("got group %q, want g1", s.Group)
		}
	}
}

// ── BulkAppendLogs tests ──────────────────────────────────────────────────────

func TestRegistry_BulkAppendLogs_Empty(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	// The enqueue timestamp must be preserved — not replaced by time.Now().
	if logs[0].Ts.UnixMilli() != 1000 {
		t.Errorf("expected enqueue TsMs=1000, got %d", logs[0].Ts.UnixMilli())
	}
}

func TestRegistry_BulkAppendLogs_Multiple(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

func TestRegistryKindedStorage(t *testing.T) {
	r := newTestRegistry(t)

	spec := makeSpec("t")
	spec.Enabled = true
	pipe := &task.PipelineTask{ID: "p", Name: "P", Subtype: "sequential", Enabled: true,
		Stages: []task.Stage{{Task: "t"}}}

	if err := r.Register(spec); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(pipe); err != nil {
		t.Fatal(err)
	}

	// GetKinded sees both kinds.
	if k, ok := r.GetKinded("p"); !ok || k.KindOf() != task.KindPipelineTask {
		t.Fatalf("GetKinded(p) = %v,%v", k, ok)
	}
	// Get (typed) returns only Task-kind.
	if _, ok := r.Get("p"); ok {
		t.Fatal("Get(p) should not return a pipeline as *task.Spec")
	}
	if s, ok := r.Get("t"); !ok || s.ID != "t" {
		t.Fatalf("Get(t) = %v,%v", s, ok)
	}
	// All (typed) filters to Task-kind; AllKinded returns both.
	if got := len(r.All()); got != 1 {
		t.Fatalf("All() = %d, want 1", got)
	}
	if got := len(r.AllKinded()); got != 2 {
		t.Fatalf("AllKinded() = %d, want 2", got)
	}
}

func TestStartRunKind(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	if _, err := r.StartRunWithID(ctx, "r1", "t1", "", "manual", RunKindPipeline); err != nil {
		t.Fatal(err)
	}
	run, err := r.GetRun(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if run.Kind != RunKindPipeline {
		t.Fatalf("Kind = %q, want %q", run.Kind, RunKindPipeline)
	}
	// Default path: StartRun wrapper writes kind=RunKindTask.
	id, err := r.StartRun(ctx, "t2", "")
	if err != nil {
		t.Fatal(err)
	}
	run2, err := r.GetRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if run2.Kind != RunKindTask {
		t.Fatalf("Kind = %q, want %q", run2.Kind, RunKindTask)
	}

	// Empty kind falls back to RunKindTask.
	if _, err := r.StartRunWithID(ctx, "r3", "t3", "", "manual", ""); err != nil {
		t.Fatal(err)
	}
	run3, err := r.GetRun(ctx, "r3")
	if err != nil {
		t.Fatal(err)
	}
	if run3.Kind != RunKindTask {
		t.Fatalf("empty kind: got %q, want %q", run3.Kind, RunKindTask)
	}
}

func TestFinishRunWithResult(t *testing.T) {
	t.Parallel()
	r := newTestRegistry(t)

	runID, err := r.StartRun(context.Background(), "task-1", "")
	if err != nil {
		t.Fatal(err)
	}

	err = r.FinishRunWithResult(context.Background(), runID, StatusSuccess,
		`{"key":"value"}`, "text/html", "<b>hello</b>")
	if err != nil {
		t.Fatal(err)
	}

	run, err := r.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != StatusSuccess {
		t.Errorf("status = %q, want %q", run.Status, StatusSuccess)
	}
	if run.ReturnValue != `{"key":"value"}` {
		t.Errorf("return_value = %q, want %q", run.ReturnValue, `{"key":"value"}`)
	}
	if run.FinishedAt == nil {
		t.Error("finished_at should be set")
	}
}

func TestSuspendRun_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newTestRegistry(t)

	runID, err := r.StartRun(ctx, "task-suspend", "")
	if err != nil {
		t.Fatal(err)
	}

	state := []byte(`{"step":"ask_name"}`)
	form := []byte(`{"fields":[{"name":"project_name"}]}`)
	token := "resume-token-xyz"
	suspendedAt := time.Now().UnixMilli()
	deadline := suspendedAt + 86_400_000

	if err := r.SuspendRun(ctx, runID, state, form, token, suspendedAt, deadline, nil); err != nil {
		t.Fatal(err)
	}

	run, err := r.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != StatusSuspended {
		t.Errorf("status = %q, want %q", run.Status, StatusSuspended)
	}
	// Suspended is non-terminal: finished_at must stay unset.
	if run.FinishedAt != nil {
		t.Errorf("finished_at = %v, want nil for a suspended run", run.FinishedAt)
	}
	if string(run.ResumeState) != string(state) {
		t.Errorf("resume_state = %q, want %q", run.ResumeState, state)
	}
	if string(run.ResumeForm) != string(form) {
		t.Errorf("resume_form = %q, want %q", run.ResumeForm, form)
	}
	if run.ResumeToken != token {
		t.Errorf("resume_token = %q, want %q", run.ResumeToken, token)
	}
	if run.SuspendedAt != suspendedAt {
		t.Errorf("suspended_at = %d, want %d", run.SuspendedAt, suspendedAt)
	}
	if run.ResumeDeadline != deadline {
		t.Errorf("resume_deadline = %d, want %d", run.ResumeDeadline, deadline)
	}
}

// A zero deadline persists as NULL and reads back as 0; nil state/form blobs
// round-trip as nil rather than a zero-length slice mismatch.
func TestSuspendRun_NoDeadlineNilBlobs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newTestRegistry(t)

	runID, err := r.StartRun(ctx, "task-suspend-2", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SuspendRun(ctx, runID, nil, nil, "tok", time.Now().UnixMilli(), 0, nil); err != nil {
		t.Fatal(err)
	}

	run, err := r.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != StatusSuspended {
		t.Errorf("status = %q, want %q", run.Status, StatusSuspended)
	}
	if run.ResumeDeadline != 0 {
		t.Errorf("resume_deadline = %d, want 0", run.ResumeDeadline)
	}
	if run.ResumeState != nil {
		t.Errorf("resume_state = %v, want nil", run.ResumeState)
	}
	if run.ResumeForm != nil {
		t.Errorf("resume_form = %v, want nil", run.ResumeForm)
	}
}

// A suspended run is not "running", so the startup stale-run sweep must leave
// it alone rather than cancelling it.
func TestCleanupStaleRuns_SkipsSuspended(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newTestRegistry(t)

	suspended, err := r.StartRun(ctx, "task-a", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SuspendRun(ctx, suspended, []byte(`{}`), nil, "tok", time.Now().UnixMilli(), 0, nil); err != nil {
		t.Fatal(err)
	}
	running, err := r.StartRun(ctx, "task-b", "")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := r.CleanupStaleRuns(ctx); err != nil {
		t.Fatal(err)
	}

	sRun, err := r.GetRun(ctx, suspended)
	if err != nil {
		t.Fatal(err)
	}
	if sRun.Status != StatusSuspended {
		t.Errorf("suspended run status = %q, want %q (sweep must skip it)", sRun.Status, StatusSuspended)
	}
	rRun, err := r.GetRun(ctx, running)
	if err != nil {
		t.Fatal(err)
	}
	if rRun.Status != StatusCancelled {
		t.Errorf("running run status = %q, want %q", rRun.Status, StatusCancelled)
	}
}
