package ipc

import (
	"context"
	"errors"
	"testing"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// recordingEngine captures the task id + params of the last FireManual call so
// the git-delete path can assert that buildin/git-pr was fired correctly.
type recordingEngine struct {
	mockEngine
	firedTask   string
	firedParams map[string]string
}

func (e *recordingEngine) FireManual(_ context.Context, taskID string, params map[string]string) (string, error) {
	e.firedTask = taskID
	e.firedParams = params
	return e.runID, e.err
}

// fakeDeleter is a test double for TaskDeleter. resolveErr / deleteErr force
// the respective failures; outcome is returned from DeleteTaskFromSource.
type fakeDeleter struct {
	resolveName string
	isGit       bool
	resolveErr  error

	outcome       TaskDeleteOutcome
	deleteErr     error
	deleteCalled  bool
	deletedTaskID string
}

func (f *fakeDeleter) ResolveTaskSource(taskID, sourceOverride string) (string, bool, error) {
	if f.resolveErr != nil {
		return "", false, f.resolveErr
	}
	name := f.resolveName
	if sourceOverride != "" {
		name = sourceOverride
	}
	return name, f.isGit, nil
}

func (f *fakeDeleter) DeleteTaskFromSource(_ context.Context, taskID, _ string, _ *task.Spec) (TaskDeleteOutcome, error) {
	f.deleteCalled = true
	f.deletedTaskID = taskID
	if f.deleteErr != nil {
		return TaskDeleteOutcome{}, f.deleteErr
	}
	return f.outcome, nil
}

func newDeleteTestServer(t *testing.T, reg *registry.Registry, eng EngineRunner, del TaskDeleter) *ControlServer {
	t.Helper()
	cs := &ControlServer{
		reg:         reg,
		engine:      eng,
		taskDeleter: del,
		log:         zap.NewNop(),
	}
	return cs
}

func newTestRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return registry.New(d)
}

func TestTaskDelete_Buildin_Undeletable(t *testing.T) {
	reg := newTestRegistry(t)
	if err := reg.Register(&task.Spec{ID: "buildin/git-pr", Name: "git-pr"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	del := &fakeDeleter{resolveName: "buildin"}
	cs := newDeleteTestServer(t, reg, &recordingEngine{}, del)

	_, err := cs.handleTaskDelete(context.Background(), Request{TaskID: "buildin/git-pr", Force: true})
	if err == nil {
		t.Fatal("expected error deleting a buildin task")
	}
	if del.deleteCalled {
		t.Fatal("DeleteTaskFromSource must not be called for a buildin task")
	}
}

func TestTaskDelete_NotFound(t *testing.T) {
	reg := newTestRegistry(t)
	cs := newDeleteTestServer(t, reg, &recordingEngine{}, &fakeDeleter{resolveName: "tasks"})

	_, err := cs.handleTaskDelete(context.Background(), Request{TaskID: "tasks/ghost", Force: true})
	if err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestTaskDelete_Preview_SurfacesChainedReferences(t *testing.T) {
	reg := newTestRegistry(t)
	target := &task.Spec{ID: "tasks/source-task", Name: "source"}
	chainer := &task.Spec{ID: "tasks/downstream", Name: "downstream"}
	chainer.Trigger.Chain = &task.ChainTrigger{From: "tasks/source-task"}
	failer := &task.Spec{ID: "tasks/notifier", Name: "notifier"}
	failer.OnFailureChain = &task.OnFailureChainSpec{Task: "tasks/source-task"}
	for _, s := range []*task.Spec{target, chainer, failer} {
		if err := reg.Register(s); err != nil {
			t.Fatalf("register %s: %v", s.ID, err)
		}
	}
	del := &fakeDeleter{resolveName: "tasks"}
	cs := newDeleteTestServer(t, reg, &recordingEngine{}, del)

	// Preview (Force=false) must not delete and must report both referrers.
	res, err := cs.handleTaskDelete(context.Background(), Request{TaskID: "tasks/source-task", Force: false})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if del.deleteCalled {
		t.Fatal("preview must not call DeleteTaskFromSource")
	}
	if res.Mode != "preview" {
		t.Fatalf("Mode = %q, want preview", res.Mode)
	}
	if len(res.Refs) != 2 {
		t.Fatalf("Refs = %v, want 2 referrers", res.Refs)
	}
	if res.Refs[0] != "tasks/downstream" || res.Refs[1] != "tasks/notifier" {
		t.Fatalf("Refs = %v, want sorted [tasks/downstream tasks/notifier]", res.Refs)
	}
}

func TestTaskDelete_Local_DeletesAndReturns(t *testing.T) {
	reg := newTestRegistry(t)
	if err := reg.Register(&task.Spec{ID: "tasks/local-task", Name: "local", TaskDir: "/tmp/x"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	del := &fakeDeleter{resolveName: "tasks", outcome: TaskDeleteOutcome{Source: "tasks", Mode: "local"}}
	eng := &recordingEngine{}
	cs := newDeleteTestServer(t, reg, eng, del)

	res, err := cs.handleTaskDelete(context.Background(), Request{TaskID: "tasks/local-task", Force: true})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !del.deleteCalled {
		t.Fatal("DeleteTaskFromSource was not called")
	}
	if res.Mode != "local" {
		t.Fatalf("Mode = %q, want local", res.Mode)
	}
	if eng.firedTask != "" {
		t.Fatalf("local delete must not fire any task, fired %q", eng.firedTask)
	}
}

func TestTaskDelete_Git_FiresGitPRTask(t *testing.T) {
	reg := newTestRegistry(t)
	if err := reg.Register(&task.Spec{ID: "tasks/git-task", Name: "git", TaskDir: "/tmp/x"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	del := &fakeDeleter{
		resolveName: "tasks",
		isGit:       true,
		outcome: TaskDeleteOutcome{
			Source:    "tasks",
			Mode:      "git",
			Branch:    "delete/tasks/git-task",
			Base:      "main",
			ClonePath: "/clones/tasks/run1",
		},
	}
	eng := &recordingEngine{}
	eng.runID = "pr-run-1"
	eng.result = RunResult{RunID: "pr-run-1", Status: "success", ReturnValue: "https://example/pr/1"}
	cs := newDeleteTestServer(t, reg, eng, del)

	res, err := cs.handleTaskDelete(context.Background(), Request{TaskID: "tasks/git-task", Force: true})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if eng.firedTask != gitPRTaskID {
		t.Fatalf("fired task = %q, want %q", eng.firedTask, gitPRTaskID)
	}
	if eng.firedParams["source_id"] != "tasks" || eng.firedParams["branch"] != "delete/tasks/git-task" {
		t.Fatalf("git-pr params wrong: %v", eng.firedParams)
	}
	if eng.firedParams["clone_path"] != "/clones/tasks/run1" {
		t.Fatalf("clone_path param = %q", eng.firedParams["clone_path"])
	}
	if res.PRValue != "https://example/pr/1" {
		t.Fatalf("PRValue = %q, want the PR url", res.PRValue)
	}
	if res.PRRunID != "pr-run-1" {
		t.Fatalf("PRRunID = %q", res.PRRunID)
	}
}

func TestTaskDelete_Git_PRTaskFailure_IsReported(t *testing.T) {
	reg := newTestRegistry(t)
	if err := reg.Register(&task.Spec{ID: "tasks/git-task", Name: "git", TaskDir: "/tmp/x"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	del := &fakeDeleter{
		resolveName: "tasks",
		isGit:       true,
		outcome:     TaskDeleteOutcome{Source: "tasks", Mode: "git", Branch: "delete/tasks/git-task", Base: "main"},
	}
	eng := &recordingEngine{}
	eng.runID = "pr-run-2"
	eng.result = RunResult{RunID: "pr-run-2", Status: "failure"}
	cs := newDeleteTestServer(t, reg, eng, del)

	_, err := cs.handleTaskDelete(context.Background(), Request{TaskID: "tasks/git-task", Force: true})
	if err == nil {
		t.Fatal("expected error when the PR task fails")
	}
}

func TestTaskDelete_ResolveError_Propagates(t *testing.T) {
	reg := newTestRegistry(t)
	if err := reg.Register(&task.Spec{ID: "tasks/x", Name: "x", TaskDir: "/tmp/x"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	del := &fakeDeleter{resolveErr: errors.New("source boom")}
	cs := newDeleteTestServer(t, reg, &recordingEngine{}, del)

	_, err := cs.handleTaskDelete(context.Background(), Request{TaskID: "tasks/x", Force: true})
	if err == nil {
		t.Fatal("expected resolve error to propagate")
	}
	if del.deleteCalled {
		t.Fatal("delete must not run when resolution fails")
	}
}
