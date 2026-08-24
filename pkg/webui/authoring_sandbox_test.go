package webui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/taskset"
	"go.uber.org/zap"
)

// registerWriteTool registers buildin/write-task-file with the given roots, so
// SandboxWritable reads them the way it reads the daemon's resolved spec.
func registerWriteTool(t *testing.T, srv *Server, roots string) {
	t.Helper()
	spec := &task.Spec{
		ID: writeTaskFileTaskID, Name: "Write Task File",
		Trigger: task.TriggerConfig{Manual: true}, Enabled: true,
	}
	spec.Permissions.Env = []task.EnvEntry{{Name: "DICODE_TASK_FILE_ROOTS", Value: roots}}
	if err := srv.registry.Register(spec); err != nil {
		t.Fatalf("register write tool: %v", err)
	}
}

// A git source resolves from the pull cache, which the next pull overwrites.
// Scaffolding there produces a task that works until an unrelated upstream
// commit deletes it, which is harder to diagnose than a refusal.
func TestCreateTask_GitSourceNotInDevModeRefuses(t *testing.T) {
	dir := t.TempDir()
	srv := newAuthoringTestServer(t, "", "")
	sm := NewSourceManager(srv.cfg, nil, srv.registry, "", zap.NewNop())
	sm.Register("upstream", taskset.NewSource("upstream", "upstream",
		&taskset.Ref{URL: "https://example.com/repo.git", Branch: "main"}, "", "", false, 0, zap.NewNop()))
	srv.sourceMgr = sm

	_, err := srv.CreateTask(context.Background(), "zen-quote", "upstream")
	if err == nil {
		t.Fatal("CreateTask into a git source succeeded, want a refusal")
	}
	var aerr *authoringError
	if !errors.As(err, &aerr) || aerr.status != 409 {
		t.Fatalf("err = %v, want a 409 authoringError", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("refused create left %d entries behind in %s", len(entries), dir)
	}
}

// SandboxPath is the boundary a session's writes are confined to. Leaving it
// empty is what made it a dead field for three releases.
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

	// It is persisted, not recomputed per call: a boundary that re-resolves
	// on every turn can move mid-session.
	sess, err := srv.authoringSessions.Get(context.Background(), res.SessionID)
	if err != nil || sess == nil {
		t.Fatalf("Get session: %v", err)
	}
	if sess.SandboxPath != want {
		t.Errorf("stored SandboxPath = %q, want %q", sess.SandboxPath, want)
	}
}

// The tool writes <root>/<task>/<file>, so being under a root is not enough —
// the depth is the boundary, and this check has to agree with the tool's own
// or the refusal it exists to pre-empt fires anyway.
func TestSandboxWritable_MirrorsToolDepthRule(t *testing.T) {
	srv := newAuthoringTestServer(t, "", "")
	registerWriteTool(t, srv, "/data/ai-tasks, /srv/authoring")

	for _, tc := range []struct {
		name string
		dir  string
		want bool
	}{
		{"directly under a root", "/data/ai-tasks/zen-quote", true},
		{"directly under a second root", "/srv/authoring/zen-quote", true},
		{"trailing slash", "/data/ai-tasks/zen-quote/", true},
		{"the root itself", "/data/ai-tasks", false},
		{"nested deeper", "/data/ai-tasks/zen-quote/sub", false},
		{"outside every root", "/tmp/dc-ai/src/zen-quote", false},
		{"prefix but not a child", "/data/ai-tasks-other/zen-quote", false},
		{"unresolved sandbox", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, roots := srv.SandboxWritable(tc.dir)
			if got != tc.want {
				t.Errorf("SandboxWritable(%q) = %v, want %v", tc.dir, got, tc.want)
			}
			if len(roots) != 2 {
				t.Errorf("roots = %v, want both declared roots for the refusal message", roots)
			}
		})
	}
}

// An unregistered write tool has no roots, so nothing is writable — the check
// fails closed rather than waving every path through.
func TestSandboxWritable_NoWriteToolRegistered(t *testing.T) {
	srv := newAuthoringTestServer(t, "", "")
	if ok, roots := srv.SandboxWritable("/data/ai-tasks/zen-quote"); ok || roots != nil {
		t.Fatalf("SandboxWritable = (%v, %v), want (false, nil)", ok, roots)
	}
}
