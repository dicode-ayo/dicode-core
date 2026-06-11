package ipc

import (
	"context"
	"errors"
	"strings"
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

	devModeDisabled bool
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

func (f *fakeDeleter) DisableSourceDevMode(_ context.Context, _ string) error {
	f.devModeDisabled = true
	return nil
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

// A pipeline whose stage references the deleted task must surface as a dangling
// referrer alongside chain/on_failure referrers.
func TestTaskDelete_Preview_SurfacesPipelineStageReference(t *testing.T) {
	reg := newTestRegistry(t)
	target := &task.Spec{ID: "tasks/source-task", Name: "source"}
	if err := reg.Register(target); err != nil {
		t.Fatalf("register target: %v", err)
	}
	pipe := &task.PipelineTask{
		ID:     "tasks/pipe",
		Kind:   task.KindPipelineTask,
		Stages: []task.Stage{{Task: "tasks/other"}, {Task: "tasks/source-task"}},
	}
	if err := reg.Register(pipe); err != nil {
		t.Fatalf("register pipeline: %v", err)
	}
	del := &fakeDeleter{resolveName: "tasks"}
	cs := newDeleteTestServer(t, reg, &recordingEngine{}, del)

	res, err := cs.handleTaskDelete(context.Background(), Request{TaskID: "tasks/source-task", Force: false})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(res.Refs) != 1 || res.Refs[0] != "tasks/pipe" {
		t.Fatalf("Refs = %v, want [tasks/pipe]", res.Refs)
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

// A registered pipeline (kind: PipelineTask) has no *task.Spec but carries a
// TaskDir, so it must resolve through the same delete path rather than erroring
// with the misleading "task not found".
func TestTaskDelete_Pipeline_DeletesViaSamePath(t *testing.T) {
	reg := newTestRegistry(t)
	pipe := &task.PipelineTask{
		ID:      "tasks/pipe",
		Kind:    task.KindPipelineTask,
		TaskDir: "/tmp/pipe",
		Stages:  []task.Stage{{Task: "tasks/a"}, {Task: "tasks/b"}},
	}
	if err := reg.Register(pipe); err != nil {
		t.Fatalf("register pipeline: %v", err)
	}
	del := &fakeDeleter{resolveName: "tasks", outcome: TaskDeleteOutcome{Source: "tasks", Mode: "local"}}
	eng := &recordingEngine{}
	cs := newDeleteTestServer(t, reg, eng, del)

	// Preview must succeed (not "task not found") and resolve the source.
	prev, err := cs.handleTaskDelete(context.Background(), Request{TaskID: "tasks/pipe", Force: false})
	if err != nil {
		t.Fatalf("pipeline preview: %v", err)
	}
	if prev.Mode != "preview" || prev.Source != "tasks" {
		t.Fatalf("preview = %+v, want mode=preview source=tasks", prev)
	}

	// Force must delete through DeleteTaskFromSource, not error.
	res, err := cs.handleTaskDelete(context.Background(), Request{TaskID: "tasks/pipe", Force: true})
	if err != nil {
		t.Fatalf("pipeline delete: %v", err)
	}
	if !del.deleteCalled {
		t.Fatal("DeleteTaskFromSource was not called for a pipeline")
	}
	if del.deletedTaskID != "tasks/pipe" {
		t.Fatalf("deleted task id = %q, want tasks/pipe", del.deletedTaskID)
	}
	if res.Mode != "local" {
		t.Fatalf("Mode = %q, want local", res.Mode)
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
	// git-pr returns an object; WaitRun unmarshals it to map[string]any. The URL
	// must be read from the "url" key, not the map's %v stringification.
	eng.result = RunResult{RunID: "pr-run-1", Status: "success", ReturnValue: map[string]any{
		"ok":  true,
		"url": "https://example/pr/1",
	}}
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
		t.Fatalf("PRValue = %q, want the PR url parsed from the map", res.PRValue)
	}
	if res.PRRunID != "pr-run-1" {
		t.Fatalf("PRRunID = %q", res.PRRunID)
	}
	if !del.devModeDisabled {
		t.Fatal("dev-mode must be disabled after a successful git delete")
	}
}

// A git-pr {ok:false} return means `gh` failed even though the run Status is
// "success"; the delete must FAIL and surface the error, not print a map.
func TestTaskDelete_Git_GitPRNotOK_Fails(t *testing.T) {
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
	eng.runID = "pr-run-3"
	eng.result = RunResult{RunID: "pr-run-3", Status: "success", ReturnValue: map[string]any{
		"ok":    false,
		"error": "gh: not authenticated",
	}}
	cs := newDeleteTestServer(t, reg, eng, del)

	_, err := cs.handleTaskDelete(context.Background(), Request{TaskID: "tasks/git-task", Force: true})
	if err == nil {
		t.Fatal("expected error when git-pr returns ok:false")
	}
	if !strings.Contains(err.Error(), "gh: not authenticated") {
		t.Fatalf("error must surface the git-pr error field, got: %v", err)
	}
	if !del.devModeDisabled {
		t.Fatal("dev-mode must be disabled even when git-pr fails")
	}
}

func TestPRURLFromReturn(t *testing.T) {
	if got, err := prURLFromReturn(map[string]any{"ok": true, "url": "u"}); err != nil || got != "u" {
		t.Fatalf("ok+url: got (%q, %v)", got, err)
	}
	if _, err := prURLFromReturn(map[string]any{"ok": true}); err == nil {
		t.Fatal("ok without url must error")
	}
	if _, err := prURLFromReturn(map[string]any{"ok": false, "error": "boom"}); err == nil {
		t.Fatal("ok:false must error")
	}
	if _, err := prURLFromReturn(map[string]any{"url": "u"}); err == nil {
		t.Fatal("missing ok must error")
	}
	if got, err := prURLFromReturn("u"); err != nil || got != "u" {
		t.Fatalf("bare string: got (%q, %v)", got, err)
	}
	if _, err := prURLFromReturn(42); err == nil {
		t.Fatal("unexpected shape must error")
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
	if !del.devModeDisabled {
		t.Fatal("dev-mode must be disabled even when the PR task fails")
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
