package webui

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/dicode/dicode/internal/gitops"
	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/taskset"
	"go.uber.org/zap"
)

func TestResolveTaskSource_FromIDPrefix(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Spec.Entries = map[string]*taskset.Entry{
		"tasks": {Ref: &taskset.Ref{Path: filepath.Join(dir, "taskset.yaml")}},
	}
	m := NewSourceManager(cfg, nil, nil, dir, zap.NewNop())
	m.Register("tasks", newStubTasksetSource(t, "tasks", dir))

	name, isGit, err := m.ResolveTaskSource("tasks/my-task", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if name != "tasks" {
		t.Fatalf("name = %q, want tasks", name)
	}
	if isGit {
		t.Fatal("local source must report isGit=false")
	}
}

func TestResolveTaskSource_GitDetected(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Spec.Entries = map[string]*taskset.Entry{
		"remote": {Ref: &taskset.Ref{URL: "https://example.com/repo.git", Branch: "main"}},
	}
	m := NewSourceManager(cfg, nil, nil, dir, zap.NewNop())
	m.Register("remote", newStubTasksetSource(t, "remote", dir))

	name, isGit, err := m.ResolveTaskSource("remote/thing", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if name != "remote" || !isGit {
		t.Fatalf("got (%q, %v), want (remote, true)", name, isGit)
	}
}

func TestResolveTaskSource_SourceOverride_Mismatch(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Spec.Entries = map[string]*taskset.Entry{
		"tasks": {Ref: &taskset.Ref{Path: filepath.Join(dir, "taskset.yaml")}},
	}
	m := NewSourceManager(cfg, nil, nil, dir, zap.NewNop())
	m.Register("tasks", newStubTasksetSource(t, "tasks", dir))

	if _, _, err := m.ResolveTaskSource("other/task", "tasks"); err == nil {
		t.Fatal("expected error: task is not under the overridden source")
	}
}

func TestResolveTaskSource_UnknownSource(t *testing.T) {
	cfg := &config.Config{}
	cfg.Spec.Entries = map[string]*taskset.Entry{}
	m := NewSourceManager(cfg, nil, nil, t.TempDir(), zap.NewNop())

	if _, _, err := m.ResolveTaskSource("ghost/task", ""); err == nil {
		t.Fatal("expected error for unknown source")
	}
}

func TestDeleteTaskFromSource_Local_RemovesDirectory(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "my-task")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "task.yaml"), []byte("name: x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg := &config.Config{}
	cfg.Spec.Entries = map[string]*taskset.Entry{
		"tasks": {Ref: &taskset.Ref{Path: filepath.Join(dir, "taskset.yaml")}},
	}
	m := NewSourceManager(cfg, nil, nil, dir, zap.NewNop())
	m.Register("tasks", newStubTasksetSource(t, "tasks", dir))

	spec := &task.Spec{ID: "tasks/my-task", TaskDir: taskDir}
	out, err := m.DeleteTaskFromSource(context.Background(), "tasks/my-task", "tasks", spec)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if out.Mode != "local" {
		t.Fatalf("Mode = %q, want local", out.Mode)
	}
	if _, statErr := os.Stat(taskDir); !os.IsNotExist(statErr) {
		t.Fatalf("task directory still exists: %v", statErr)
	}
}

// An entry left pointing at a removed directory resolves as a load failure on
// every sync, so deleting a task must take its taskset entry with it.
func TestDeleteTaskFromSource_Local_RemovesTasksetEntry(t *testing.T) {
	dir := t.TempDir()
	taskDir := filepath.Join(dir, "my-task")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "task.yaml"), []byte("name: x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg := &config.Config{}
	cfg.Spec.Entries = map[string]*taskset.Entry{
		"tasks": {Ref: &taskset.Ref{Path: filepath.Join(dir, "taskset.yaml")}},
	}
	m := NewSourceManager(cfg, nil, nil, dir, zap.NewNop())
	src := newStubTasksetSource(t, "tasks", dir)
	m.Register("tasks", src)
	if err := taskset.AddTaskEntry(filepath.Join(dir, "taskset.yaml"), "my-task"); err != nil {
		t.Fatalf("seed entry: %v", err)
	}

	spec := &task.Spec{ID: "tasks/my-task", TaskDir: taskDir}
	if _, err := m.DeleteTaskFromSource(context.Background(), "tasks/my-task", "tasks", spec); err != nil {
		t.Fatalf("delete: %v", err)
	}

	ts, err := taskset.LoadTaskSet(filepath.Join(dir, "taskset.yaml"))
	if err != nil {
		t.Fatalf("load taskset: %v", err)
	}
	if _, still := ts.Spec.Entries["my-task"]; still {
		t.Fatalf("entry survived the delete: %v", ts.Spec.Entries)
	}
}

// A TaskDir that escapes the source root must be refused — defends against a
// stale or crafted spec removing arbitrary directories.
func TestDeleteTaskFromSource_Local_RefusesEscape(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	escapeDir := filepath.Join(outside, "victim")
	if err := os.MkdirAll(escapeDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cfg := &config.Config{}
	cfg.Spec.Entries = map[string]*taskset.Entry{
		"tasks": {Ref: &taskset.Ref{Path: filepath.Join(dir, "taskset.yaml")}},
	}
	m := NewSourceManager(cfg, nil, nil, dir, zap.NewNop())
	m.Register("tasks", newStubTasksetSource(t, "tasks", dir))

	spec := &task.Spec{ID: "tasks/evil", TaskDir: escapeDir}
	if _, err := m.DeleteTaskFromSource(context.Background(), "tasks/evil", "tasks", spec); err == nil {
		t.Fatal("expected refusal when TaskDir escapes source root")
	}
	if _, statErr := os.Stat(escapeDir); statErr != nil {
		t.Fatalf("escape dir must NOT have been removed: %v", statErr)
	}
}

// A task dir that is a symlink pointing outside the source root must be refused:
// the lexical path sits under root, but os.RemoveAll would follow the link out.
func TestDeleteTaskFromSource_Local_RefusesSymlinkEscape(t *testing.T) {
	if _, err := os.Lstat("/"); err != nil {
		t.Skip("filesystem unavailable")
	}
	dir := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatalf("mkdir victim: %v", err)
	}
	if err := os.WriteFile(filepath.Join(victim, "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	// A symlink inside the source root pointing at the outside victim dir.
	link := filepath.Join(dir, "evil")
	if err := os.Symlink(victim, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	cfg := &config.Config{}
	cfg.Spec.Entries = map[string]*taskset.Entry{
		"tasks": {Ref: &taskset.Ref{Path: filepath.Join(dir, "taskset.yaml")}},
	}
	m := NewSourceManager(cfg, nil, nil, dir, zap.NewNop())
	m.Register("tasks", newStubTasksetSource(t, "tasks", dir))

	spec := &task.Spec{ID: "tasks/evil", TaskDir: link}
	if _, err := m.DeleteTaskFromSource(context.Background(), "tasks/evil", "tasks", spec); err == nil {
		t.Fatal("expected refusal when TaskDir is a symlink escaping the source root")
	}
	if _, statErr := os.Stat(filepath.Join(victim, "keep.txt")); statErr != nil {
		t.Fatalf("symlink target must NOT have been removed: %v", statErr)
	}
}

// gitFixtureRemote builds a bare git remote seeded with the given files on
// `branch` and returns a file:// URL usable as a taskset git ref. Mirrors the
// taskset package's own fixture helper.
func gitFixtureRemote(t *testing.T, branch string, files map[string]string) string {
	t.Helper()
	bareDir := filepath.Join(t.TempDir(), "bare.git")
	if _, err := gogit.PlainInitWithOptions(bareDir, &gogit.PlainInitOptions{
		Bare:        true,
		InitOptions: gogit.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName(branch)},
	}); err != nil {
		t.Fatalf("init bare: %v", err)
	}
	wtPath := filepath.Join(t.TempDir(), "seed-wt")
	wt, err := gogit.PlainInitWithOptions(wtPath, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName(branch)},
	})
	if err != nil {
		t.Fatalf("init wt: %v", err)
	}
	if _, err := wt.CreateRemote(&gogitconfig.RemoteConfig{Name: "origin", URLs: []string{bareDir}}); err != nil {
		t.Fatalf("create remote: %v", err)
	}
	tree, err := wt.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	for name, content := range files {
		full := filepath.Join(wtPath, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if _, err := tree.Add(name); err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
	}
	if _, err := tree.Commit("initial", &gogit.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@t", When: time.Now()},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := wt.Push(&gogit.PushOptions{RemoteName: "origin"}); err != nil && err != gogit.NoErrAlreadyUpToDate {
		t.Fatalf("push: %v", err)
	}
	// See gitops.TestFixtureRemoteURL's doc comment for why this needs a
	// placeholder hostname rather than a bare file:// path.
	return gitops.TestFixtureRemoteURL(bareDir)
}

// TestDeleteTaskFromSource_Git_ClonePushesDeleteBranch exercises the real
// git-source path: clone the source, remove the task dir, commit, push to
// delete/<id>. It asserts the bare remote ends up with the delete branch and
// that the task file is gone from that branch's tree. The PR-opening step is
// the control server's job (covered in pkg/ipc) and is out of scope here.
func TestDeleteTaskFromSource_Git_ClonePushesDeleteBranch(t *testing.T) {
	remoteURL := gitFixtureRemote(t, "main", map[string]string{
		"taskset.yaml": "apiVersion: dicode/v1\nkind: TaskSet\nspec:\n  entries:\n" +
			"    my-task:\n      ref:\n        path: ./my-task/task.yaml\n" +
			"    keep-task:\n      ref:\n        path: ./keep-task/task.yaml\n",
		"my-task/task.yaml":   "apiVersion: dicode/v1\nkind: Task\nname: my-task\nruntime: deno\ntrigger:\n  manual: true\n",
		"my-task/task.ts":     "export default async function () {}\n",
		"keep-task/task.yaml": "apiVersion: dicode/v1\nkind: Task\nname: keep\nruntime: deno\ntrigger:\n  manual: true\n",
		"keep-task/task.ts":   "export default async function () {}\n",
	})

	dataDir := t.TempDir()
	cfg := &config.Config{}
	cfg.Spec.Entries = map[string]*taskset.Entry{
		"remote": {Ref: &taskset.Ref{URL: remoteURL, Branch: "main"}},
	}
	m := NewSourceManager(cfg, nil, nil, dataDir, zap.NewNop())

	src := taskset.NewSource(remoteURL, "remote", &taskset.Ref{URL: remoteURL, Branch: "main"}, "", dataDir, false, 30*time.Second, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if _, err := src.Start(ctx); err != nil {
		t.Fatalf("start git source: %v", err)
	}
	m.Register("remote", src)

	primaryRoot := src.RepoPath()
	spec := &task.Spec{ID: "remote/my-task", TaskDir: filepath.Join(primaryRoot, "my-task")}

	out, err := m.DeleteTaskFromSource(ctx, "remote/my-task", "remote", spec)
	if err != nil {
		t.Fatalf("git delete: %v", err)
	}
	if out.Mode != "git" {
		t.Fatalf("Mode = %q, want git", out.Mode)
	}
	if out.Branch != "delete/remote/my-task" {
		t.Fatalf("Branch = %q, want delete/remote/my-task", out.Branch)
	}
	if out.ClonePath == "" {
		t.Fatal("ClonePath should be set for the PR task")
	}

	// Verify the bare remote now has the delete branch with my-task removed.
	verifyDir := filepath.Join(t.TempDir(), "verify")
	vr, err := gogit.PlainCloneContext(ctx, verifyDir, false, &gogit.CloneOptions{
		URL:           remoteURL,
		ReferenceName: plumbing.NewBranchReferenceName("delete/remote/my-task"),
	})
	if err != nil {
		t.Fatalf("clone delete branch: %v", err)
	}
	head, err := vr.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	commit, err := vr.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("commit object: %v", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	if _, err := tree.File("my-task/task.yaml"); err == nil {
		t.Fatal("my-task/task.yaml should be gone on the delete branch")
	}
	if _, err := tree.File("keep-task/task.yaml"); err != nil {
		t.Fatalf("keep-task/task.yaml should still exist: %v", err)
	}
}

func TestSanitizeRunID(t *testing.T) {
	cases := map[string]string{
		"tasks/my-task": "tasks_my-task",
		"a/b/c":         "a_b_c",
		"weird name!@#": "weird_name___",
		"":              "delete",
		"keep_-AZ09":    "keep_-AZ09",
	}
	for in, want := range cases {
		if got := sanitizeRunID(in); got != want {
			t.Errorf("sanitizeRunID(%q) = %q, want %q", in, got, want)
		}
	}
	long := make([]byte, 100)
	for i := range long {
		long[i] = 'x'
	}
	if got := sanitizeRunID(string(long)); len(got) != 64 {
		t.Errorf("sanitizeRunID truncation: len = %d, want 64", len(got))
	}
}
