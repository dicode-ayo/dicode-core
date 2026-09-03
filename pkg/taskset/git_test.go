package taskset

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/dicode/dicode/internal/gitops"
	"github.com/dicode/dicode/pkg/source"
	"go.uber.org/zap"
)

// seededBareRepo is a bare repo on disk plus a scratch worktree used to
// push commits, mirroring the pattern in pkg/source/git/git_test.go.
type seededBareRepo struct {
	bareDir string
	url     string
	wt      *gogit.Repository
	wtPath  string
	branch  string
}

func newSeededBareRepo(t *testing.T) *seededBareRepo {
	t.Helper()

	bareDir := filepath.Join(t.TempDir(), "bare.git")
	if _, err := gogit.PlainInitWithOptions(bareDir, &gogit.PlainInitOptions{
		Bare:        true,
		InitOptions: gogit.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName("main")},
	}); err != nil {
		t.Fatalf("init bare: %v", err)
	}

	wtPath := filepath.Join(t.TempDir(), "seed-wt")
	wt, err := gogit.PlainInitWithOptions(wtPath, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName("main")},
	})
	if err != nil {
		t.Fatalf("init wt: %v", err)
	}
	if _, err := wt.CreateRemote(&gogitconfig.RemoteConfig{
		Name: "origin",
		URLs: []string{bareDir},
	}); err != nil {
		t.Fatalf("create remote: %v", err)
	}

	return &seededBareRepo{
		bareDir: bareDir,
		// See gitops.TestFixtureRemoteURL's doc comment for why this needs a
		// placeholder hostname rather than a bare file:// path.
		url:    gitops.TestFixtureRemoteURL(bareDir),
		wt:     wt,
		wtPath: wtPath,
		branch: "main",
	}
}

func (r *seededBareRepo) commit(t *testing.T, filename, body, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.wtPath, filename), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	tree, err := r.wt.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if _, err := tree.Add(filename); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := tree.Commit(msg, &gogit.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@t", When: time.Now()},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := r.wt.Push(&gogit.PushOptions{RemoteName: "origin"}); err != nil && err != gogit.NoErrAlreadyUpToDate {
		t.Fatalf("push: %v", err)
	}
}

// countCommits walks HEAD and returns how many commits are reachable.
// A shallow clone (Depth:1) reports 1 regardless of upstream history.
func countCommits(t *testing.T, dir string) int {
	t.Helper()
	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		t.Fatalf("open clone: %v", err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	iter, err := repo.Log(&gogit.LogOptions{From: head.Hash()})
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	n := 0
	_ = iter.ForEach(func(*object.Commit) error { n++; return nil })
	return n
}

// TestCloneOrPull_FetchesFullHistory guards against the #175 regression:
// a shallow (Depth:1) clone silently stalls on `object not found` when
// the remote advances past the shallow tip. A full clone has the
// ancestry it needs to fast-forward cleanly. If this test sees only 1
// commit the clone has reverted to shallow.
func TestCloneOrPull_FetchesFullHistory(t *testing.T) {
	bare := newSeededBareRepo(t)
	bare.commit(t, "a", "one", "commit 1")
	bare.commit(t, "b", "two", "commit 2")
	bare.commit(t, "c", "three", "commit 3")

	clone := filepath.Join(t.TempDir(), "clone")
	if err := syncClone(context.Background(), clone, bare.url, gitTarget{Kind: refBranch, Name: "main"}, ""); err != nil {
		t.Fatalf("syncClone: %v", err)
	}

	if got := countCommits(t, clone); got < 3 {
		t.Errorf("clone has %d commits; want >=3 (shallow clone would report 1)", got)
	}
}

// TestCloneOrPull_RecoversCorruptedClone reproduces the shape users
// hit when upgrading from a dicode build that did shallow clones
// (Depth: 1) to a current build: the on-disk clone is stuck in a
// state go-git's PullContext can't reconcile, and every subsequent
// pull fails with a reconcile-style error ("pull: object not found")
// no matter how long you wait.
//
// syncClone must detect that failure class, wipe the directory, and
// re-clone from scratch — otherwise operators have to manually
// `rm -rf ~/.dicode/tasksets` after every upgrade.
//
// The local `file://` transport handles shallow-pulls gracefully
// (it has direct access to the remote's object DB), so we can't
// reproduce the exact HTTPS shallow-stuck state here. Instead we
// corrupt the clone's object DB — deleting the packfiles forces
// go-git's pull to fail with a missing-object error of the same
// signature.
func TestCloneOrPull_RecoversCorruptedClone(t *testing.T) {
	bare := newSeededBareRepo(t)
	bare.commit(t, "a", "one", "commit 1")

	clone := filepath.Join(t.TempDir(), "clone")
	if err := syncClone(context.Background(), clone, bare.url, gitTarget{Kind: refBranch, Name: "main"}, ""); err != nil {
		t.Fatalf("initial clone: %v", err)
	}

	// Corrupt the clone: delete every object (loose + packed) so any
	// HEAD ref becomes unresolvable. The next pull will fail with a
	// missing-object style error — the same shape as the production
	// shallow-stuck error.
	for _, sub := range []string{"objects/pack", "objects"} {
		p := filepath.Join(clone, ".git", sub)
		entries, _ := os.ReadDir(p)
		for _, e := range entries {
			if e.Name() == "info" {
				continue
			}
			_ = os.RemoveAll(filepath.Join(p, e.Name()))
		}
	}

	// Advance the remote so the pull has something to do.
	bare.commit(t, "b", "two", "commit 2")
	bare.commit(t, "c", "three", "commit 3")

	// syncClone must detect the broken clone and recover.
	if err := syncClone(context.Background(), clone, bare.url, gitTarget{Kind: refBranch, Name: "main"}, ""); err != nil {
		t.Fatalf("syncClone did not recover from corrupted clone: %v", err)
	}

	if got := countCommits(t, clone); got < 3 {
		t.Errorf("after recovery, clone has %d commits; want >=3 (recovery should have re-cloned full history)", got)
	}
}

// TestIsReclonableError spot-checks the error-signature heuristic used
// by syncClone to decide whether to wipe and re-clone vs propagate.
// Keeps the production error messages observed in the wild locked in.
func TestIsReclonableError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "shallow-stuck pull (the production signature from #175)",
			err:  errorsNew("pull: object not found"),
			want: true,
		},
		{
			name: "pack missing after corruption",
			err:  errorsNew("pull: packfile: not found"),
			want: true,
		},
		{
			name: "reference resolution failure",
			err:  errorsNew("pull: reference not found"),
			want: true,
		},
		{
			name: "network error — don't reclone, just retry next tick",
			err:  errorsNew("pull: dial tcp: connection refused"),
			want: false,
		},
		{
			name: "auth failure — reclone won't help",
			err:  errorsNew("pull: authentication required"),
			want: false,
		},
	}
	for _, tc := range cases {
		if got := isReclonableError(tc.err); got != tc.want {
			t.Errorf("%s: isReclonableError(%q) = %v; want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

func errorsNew(s string) error { return fmt.Errorf("%s", s) }

// TestCloneOrPull_PullAfterRemoteAdvance ensures the second call — the
// pull path — succeeds against a remote that has received new commits
// since the initial clone. Under the old Depth:1 scheme this was the
// exact path that produced "pull: object not found" in production.
func TestCloneOrPull_PullAfterRemoteAdvance(t *testing.T) {
	bare := newSeededBareRepo(t)
	bare.commit(t, "a", "one", "commit 1")

	clone := filepath.Join(t.TempDir(), "clone")
	if err := syncClone(context.Background(), clone, bare.url, gitTarget{Kind: refBranch, Name: "main"}, ""); err != nil {
		t.Fatalf("initial syncClone: %v", err)
	}

	bare.commit(t, "b", "two", "commit 2")
	bare.commit(t, "c", "three", "commit 3")

	if err := syncClone(context.Background(), clone, bare.url, gitTarget{Kind: refBranch, Name: "main"}, ""); err != nil {
		t.Fatalf("pull after remote advance: %v", err)
	}

	if got := countCommits(t, clone); got < 3 {
		t.Errorf("after pull, clone has %d commits; want >=3", got)
	}
}

// commitTree writes every path in files (relative, subdirectories created as
// needed), commits them, and pushes to the bare remote.
func (r *seededBareRepo) commitTree(t *testing.T, files map[string]string, msg string) {
	t.Helper()
	tree, err := r.wt.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	for name, body := range files {
		full := filepath.Join(r.wtPath, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if _, err := tree.Add(name); err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
	}
	if _, err := tree.Commit(msg, &gogit.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@t", When: time.Now()},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := r.wt.Push(&gogit.PushOptions{RemoteName: "origin"}); err != nil && err != gogit.NoErrAlreadyUpToDate {
		t.Fatalf("push: %v", err)
	}
}

// tagHead tags the current HEAD and pushes the tag.
func (r *seededBareRepo) tagHead(t *testing.T, name string) {
	t.Helper()
	head, err := r.wt.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if _, err := r.wt.CreateTag(name, head.Hash(), nil); err != nil {
		t.Fatalf("create tag: %v", err)
	}
	err = r.wt.Push(&gogit.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []gogitconfig.RefSpec{"+refs/tags/*:refs/tags/*"},
	})
	if err != nil && err != gogit.NoErrAlreadyUpToDate {
		t.Fatalf("push tag: %v", err)
	}
}

func tasksetRepoFiles(cron string) map[string]string {
	return map[string]string{
		"taskset.yaml":           "apiVersion: dicode/v1\nkind: TaskSet\nmetadata:\n  name: infra\nspec:\n  entries:\n    deploy:\n      ref:\n        path: tasks/deploy/task.yaml\n",
		"tasks/deploy/task.yaml": "apiVersion: dicode/v1\nkind: Task\nname: deploy\nruntime: deno\ntrigger:\n  cron: \"" + cron + "\"\n",
		"tasks/deploy/task.js":   "// task",
	}
}

// TestSource_PinnedTagDoesNotFollowTheBranch is #825's headline guarantee: a
// source pinned to a tag registers the tagged commit's tasks and stays there
// while the branch it was cut from moves on underneath it.
func TestSource_PinnedTagDoesNotFollowTheBranch(t *testing.T) {
	bare := newSeededBareRepo(t)
	bare.commitTree(t, tasksetRepoFiles("0 8 * * *"), "release")
	bare.tagHead(t, "v1.0.0")
	bare.commitTree(t, tasksetRepoFiles("0 9 * * *"), "move on")

	ref := &Ref{URL: bare.url, Tag: "v1.0.0", Path: "taskset.yaml", PollInterval: 50 * time.Millisecond}
	src := NewSource("pinned", "infra", ref, "", t.TempDir(), false, 50*time.Millisecond, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := src.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Long enough for several poll intervals: an unpinned source would have
	// pulled the newer commit and emitted an update by now.
	events := collectEvents(t, ch, 500*time.Millisecond)
	if len(events) != 1 {
		t.Fatalf("got %d events, want exactly the one Added for the pinned commit: %+v", len(events), events)
	}
	ev := events[0]
	if ev.Kind != source.EventAdded || ev.TaskID != "infra/deploy" {
		t.Fatalf("got %v for %q, want EventAdded for infra/deploy", ev.Kind, ev.TaskID)
	}
	if got := asSpec(ev.Kinded).Trigger.Cron; got != "0 8 * * *" {
		t.Errorf("resolved cron = %q, want the tagged commit's %q", got, "0 8 * * *")
	}

	ps := src.PullStatus()
	if !ps.Pinned {
		t.Error("PullStatus().Pinned = false on a tag-pinned source")
	}
	if !ps.OK || ps.LastPullAt.IsZero() {
		t.Errorf("PullStatus() = %+v, want a recorded successful resolve", ps)
	}
}

// TestSource_UnknownTagReportsItAndRegistersNothing covers the operator-typo
// path end to end: the source comes up, reports the failure through the health
// surface the webui reads, and registers no tasks.
func TestSource_UnknownTagReportsItAndRegistersNothing(t *testing.T) {
	bare := newSeededBareRepo(t)
	bare.commitTree(t, tasksetRepoFiles("0 8 * * *"), "release")
	bare.tagHead(t, "v1.0.0")

	ref := &Ref{URL: bare.url, Tag: "v9.9.9", Path: "taskset.yaml", PollInterval: 50 * time.Millisecond}
	src := NewSource("pinned", "infra", ref, "", t.TempDir(), false, 50*time.Millisecond, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := src.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if events := collectEvents(t, ch, 300*time.Millisecond); len(events) != 0 {
		t.Fatalf("got %d events for an unresolvable tag, want none: %+v", len(events), events)
	}
	ps := src.PullStatus()
	if ps.OK {
		t.Error("PullStatus().OK = true for an unknown tag")
	}
	if !strings.Contains(ps.Error, "v9.9.9") {
		t.Errorf("PullStatus().Error = %q, want it to name the missing tag", ps.Error)
	}
}
