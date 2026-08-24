package webui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/taskset"
	"go.uber.org/zap"
)

// registerWriteTool registers buildin/write-task-file with the given grants,
// so the authorability checks read them the way they read a resolved spec.
// An empty roots string leaves the env entry off entirely.
func registerWriteTool(t *testing.T, srv *Server, fsPaths []string, roots string) {
	t.Helper()
	spec := &task.Spec{
		ID: writeTaskFileTaskID, Name: "Write Task File",
		Trigger: task.TriggerConfig{Manual: true}, Enabled: true,
	}
	for _, p := range fsPaths {
		spec.Permissions.FS = append(spec.Permissions.FS, task.FSEntry{Path: p, Permission: "rw"})
	}
	if roots != "" {
		spec.Permissions.Env = []task.EnvEntry{{Name: taskFileRootsEnv, Value: roots}}
	}
	if err := srv.registry.Register(spec); err != nil {
		t.Fatalf("register write tool: %v", err)
	}
}

// registerGitSource wires a git-backed source that is not in dev mode.
func registerGitSource(t *testing.T, srv *Server, name string) {
	t.Helper()
	sm := NewSourceManager(srv.cfg, nil, srv.registry, "", zap.NewNop())
	sm.Register(name, taskset.NewSource(name, name,
		&taskset.Ref{URL: "https://example.com/repo.git", Branch: "main"}, "", "", false, 0, zap.NewNop()))
	srv.sourceMgr = sm
}

// A git source resolves from the pull cache, which the next pull overwrites.
// Scaffolding there produces a task that works until an unrelated upstream
// commit deletes it, which is harder to diagnose than a refusal.
func TestCreateTask_GitSourceNotInDevModeRefuses(t *testing.T) {
	srv := newAuthoringTestServer(t, "", "")
	registerGitSource(t, srv, "upstream")

	_, err := srv.CreateTask(context.Background(), "zen-quote", "upstream")
	if err == nil {
		t.Fatal("CreateTask into a git source succeeded, want a refusal")
	}
	var aerr *authoringError
	if !errors.As(err, &aerr) || aerr.status != 409 {
		t.Fatalf("err = %v, want a 409 authoringError", err)
	}
	if !strings.Contains(err.Error(), "pull cache") {
		t.Errorf("err = %v, want the pull-cache explanation", err)
	}
}

// A local source's scaffold is durable, so the scaffold gate must not fire on
// it — the check exists to stop pull-cache writes, not authoring at large.
func TestCreateTask_LocalSourceStillScaffolds(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "scratch", dir)

	res, err := srv.CreateTask(context.Background(), "zen-quote", "scratch")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if res.TaskID != "scratch/zen-quote" {
		t.Errorf("task id = %q", res.TaskID)
	}
	if _, err := os.Stat(filepath.Join(dir, "zen-quote", "task.yaml")); err != nil {
		t.Errorf("scaffold did not land: %v", err)
	}
}

// SandboxPath is the directory the gate checks and the model is aimed at.
// Leaving it unassigned is what made it a dead field.
func TestEditTask_PopulatesSandboxPath(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "ai-scratch", dir)

	res, err := srv.EditTask(context.Background(), "", "ai-scratch/hello")
	if err != nil {
		t.Fatalf("EditTask: %v", err)
	}
	want := filepath.Join(dir, "hello")
	if res.SandboxPath != want {
		t.Fatalf("SandboxPath = %q, want %q", res.SandboxPath, want)
	}

	sess, err := srv.authoringSessions.Get(context.Background(), res.SessionID)
	if err != nil || sess == nil {
		t.Fatalf("Get session: %v", err)
	}
	if sess.SandboxPath != want {
		t.Errorf("stored SandboxPath = %q, want %q", sess.SandboxPath, want)
	}
}

// A session row written before sandbox_path was ever assigned has to resolve
// to something, or resuming it refuses every turn while naming "".
func TestEditTask_ResumeBackfillsEmptySandboxPath(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "ai-scratch", dir)
	ctx := context.Background()

	now := time.Now()
	legacy := AuthoringSession{
		ID: "legacy-1", Kind: "edit", Source: "ai-scratch", TaskID: "ai-scratch/hello",
		CreatedAt: now, LastTurnAt: now,
	}
	if err := srv.authoringSessions.Create(ctx, legacy); err != nil {
		t.Fatalf("seed legacy session: %v", err)
	}

	res, err := srv.EditTask(ctx, "legacy-1", "")
	if err != nil {
		t.Fatalf("EditTask: %v", err)
	}
	if want := filepath.Join(dir, "hello"); res.SandboxPath != want {
		t.Errorf("resumed SandboxPath = %q, want %q", res.SandboxPath, want)
	}
}

// The runtime's write permission is a prefix grant, so this half of the check
// has to be a prefix test — a task whose entry points at a nested path is
// still writable.
func TestCheckSessionAuthorable_FSGrantIsAPrefix(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "ai-scratch", dir)
	registerWriteTool(t, srv, []string{"/data/ai-tasks"}, "/data/ai-tasks")

	if err := srv.CheckSessionAuthorable("ai-scratch", "/data/ai-tasks/zen"); err != nil {
		t.Errorf("directly under the grant: %v", err)
	}
	if err := srv.CheckSessionAuthorable("ai-scratch", "/tmp/elsewhere/zen"); err == nil {
		t.Error("outside the grant was allowed")
	} else if !strings.Contains(err.Error(), "fs grant") {
		t.Errorf("err = %v, want it to name the fs grant", err)
	}
}

// Two grants have to agree for a write to land. Widening only one leaves a
// turn that fails inside the tool loop, which is what the gate exists to stop.
func TestCheckSessionAuthorable_RequiresBothGrants(t *testing.T) {
	dir := t.TempDir()

	t.Run("fs widened, roots not", func(t *testing.T) {
		srv := newAuthoringTestServer(t, "ai-scratch", dir)
		registerWriteTool(t, srv, []string{"/data/ai-tasks", "/srv/mine"}, "/data/ai-tasks")
		err := srv.CheckSessionAuthorable("ai-scratch", "/srv/mine/zen")
		if err == nil {
			t.Fatal("allowed a path the tool's own roots exclude")
		}
		if !strings.Contains(err.Error(), taskFileRootsEnv) {
			t.Errorf("err = %v, want it to name %s", err, taskFileRootsEnv)
		}
	})

	t.Run("roots widened, fs not", func(t *testing.T) {
		srv := newAuthoringTestServer(t, "ai-scratch", dir)
		registerWriteTool(t, srv, []string{"/data/ai-tasks"}, "/data/ai-tasks,/srv/mine")
		err := srv.CheckSessionAuthorable("ai-scratch", "/srv/mine/zen")
		if err == nil {
			t.Fatal("allowed a path the runtime would refuse to write")
		}
		if !strings.Contains(err.Error(), "fs grant") {
			t.Errorf("err = %v, want it to name the fs grant", err)
		}
	})

	t.Run("both widened", func(t *testing.T) {
		srv := newAuthoringTestServer(t, "ai-scratch", dir)
		registerWriteTool(t, srv, []string{"/data/ai-tasks", "/srv/mine"}, "/data/ai-tasks,/srv/mine")
		if err := srv.CheckSessionAuthorable("ai-scratch", "/srv/mine/zen"); err != nil {
			t.Errorf("both grants widened, still refused: %v", err)
		}
	})
}

// The roots value is matched by the tool as a raw string prefix. Normalising
// it here would accept a root the tool then rejects, which is the same green
// run that wrote nothing the gate exists to prevent.
func TestCheckSessionAuthorable_MirrorsToolRootMatching(t *testing.T) {
	dir := t.TempDir()

	for _, tc := range []struct {
		name    string
		roots   string
		dir     string
		allowed bool
	}{
		{"directly under a root", "/data/ai-tasks", "/data/ai-tasks/zen", true},
		{"trailing slash on the root", "/data/ai-tasks/", "/data/ai-tasks/zen", true},
		{"the root itself", "/data/ai-tasks", "/data/ai-tasks", false},
		{"nested deeper than a task", "/data/ai-tasks", "/data/ai-tasks/zen/sub", false},
		{"prefix but not a child", "/data/ai-tasks", "/data/ai-tasks-other/zen", false},
		// The tool trims trailing slashes and nothing else, so a doubled
		// separator inside the root never matches a real path.
		{"unclean root the tool would reject", "/data//ai-tasks", "/data/ai-tasks/zen", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newAuthoringTestServer(t, "ai-scratch", dir)
			// The fs grant is deliberately wide here so the roots half is
			// what decides each case.
			registerWriteTool(t, srv, []string{"/data", "/"}, tc.roots)
			err := srv.CheckSessionAuthorable("ai-scratch", tc.dir)
			if tc.allowed && err != nil {
				t.Errorf("CheckSessionAuthorable(%q) = %v, want allowed", tc.dir, err)
			}
			if !tc.allowed && err == nil {
				t.Errorf("CheckSessionAuthorable(%q) = nil, want refused", tc.dir)
			}
		})
	}
}

// Roots supplied via `from:` or `secret:` resolve at dispatch, so the spec
// cannot say what they are. Refusing on that would reject a configuration
// that works.
func TestCheckSessionAuthorable_RootsInjectedDynamicallyDoNotRefuse(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "ai-scratch", dir)

	spec := &task.Spec{
		ID: writeTaskFileTaskID, Name: "Write Task File",
		Trigger: task.TriggerConfig{Manual: true}, Enabled: true,
	}
	spec.Permissions.FS = []task.FSEntry{{Path: "/srv/mine", Permission: "rw"}}
	spec.Permissions.Env = []task.EnvEntry{{Name: taskFileRootsEnv, From: "MY_ROOTS"}}
	if err := srv.registry.Register(spec); err != nil {
		t.Fatalf("register: %v", err)
	}

	if err := srv.CheckSessionAuthorable("ai-scratch", "/srv/mine/zen"); err != nil {
		t.Errorf("refused a dynamically-injected roots config: %v", err)
	}
}

// With no write tool there is no route to disk at all, and the refusal has to
// say that rather than point at an empty grant list.
func TestCheckSessionAuthorable_NoWriteToolRegistered(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "ai-scratch", dir)

	err := srv.CheckSessionAuthorable("ai-scratch", "/data/ai-tasks/zen")
	if err == nil {
		t.Fatal("allowed a turn with no write tool registered")
	}
	if !strings.Contains(err.Error(), "not registered") || strings.Contains(err.Error(), "[]") {
		t.Errorf("err = %v, want the not-registered wording and no empty list", err)
	}
}

// An unresolved sandbox has nowhere to check and nowhere to write, and the
// remedy is a new session rather than a grant.
func TestCheckSessionAuthorable_UnresolvedSandbox(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "ai-scratch", dir)
	registerWriteTool(t, srv, []string{"/data/ai-tasks"}, "/data/ai-tasks")

	err := srv.CheckSessionAuthorable("ai-scratch", "")
	if err == nil {
		t.Fatal("allowed a turn with no resolved directory")
	}
	if !strings.Contains(err.Error(), "cancel it and open a new one") {
		t.Errorf("err = %v, want the cancel-and-reopen remedy", err)
	}
}

// An edit against a git source must meet the pull-cache explanation, not a
// grants message telling the operator to widen roots into the pull cache.
func TestCheckSessionAuthorable_GitSourceGetsThePullCacheReason(t *testing.T) {
	srv := newAuthoringTestServer(t, "", "")
	registerGitSource(t, srv, "upstream")
	registerWriteTool(t, srv, []string{"/data/ai-tasks"}, "/data/ai-tasks")

	err := srv.CheckSessionAuthorable("upstream", "/data/ai-tasks/zen")
	if err == nil {
		t.Fatal("allowed a turn against a git source not in dev mode")
	}
	if !strings.Contains(err.Error(), "pull cache") {
		t.Errorf("err = %v, want the pull-cache explanation", err)
	}
}

// The scaffold pre-flight answers for a task that does not exist yet, so it
// has to decide from the source's own root.
func TestCheckSourceAuthorable(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "scratch", dir)

	registerWriteTool(t, srv, []string{dir}, dir)
	if err := srv.CheckSourceAuthorable("scratch"); err != nil {
		t.Errorf("source rooted at a granted path was refused: %v", err)
	}

	srv2 := newAuthoringTestServer(t, "scratch", dir)
	registerWriteTool(t, srv2, []string{"/data/ai-tasks"}, "/data/ai-tasks")
	if err := srv2.CheckSourceAuthorable("scratch"); err == nil {
		t.Error("source outside every grant was allowed")
	}
}
