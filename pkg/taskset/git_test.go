package taskset

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
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
		url:     "file://" + bareDir,
		wt:      wt,
		wtPath:  wtPath,
		branch:  "main",
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
	if err := cloneOrPull(context.Background(), clone, bare.url, "main", ""); err != nil {
		t.Fatalf("cloneOrPull: %v", err)
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
// cloneOrPull must detect that failure class, wipe the directory, and
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
	if err := cloneOrPull(context.Background(), clone, bare.url, "main", ""); err != nil {
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

	// cloneOrPull must detect the broken clone and recover.
	if err := cloneOrPull(context.Background(), clone, bare.url, "main", ""); err != nil {
		t.Fatalf("cloneOrPull did not recover from corrupted clone: %v", err)
	}

	if got := countCommits(t, clone); got < 3 {
		t.Errorf("after recovery, clone has %d commits; want >=3 (recovery should have re-cloned full history)", got)
	}
}

// TestIsReclonableError spot-checks the error-signature heuristic used
// by cloneOrPull to decide whether to wipe and re-clone vs propagate.
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
	if err := cloneOrPull(context.Background(), clone, bare.url, "main", ""); err != nil {
		t.Fatalf("initial cloneOrPull: %v", err)
	}

	bare.commit(t, "b", "two", "commit 2")
	bare.commit(t, "c", "three", "commit 3")

	if err := cloneOrPull(context.Background(), clone, bare.url, "main", ""); err != nil {
		t.Fatalf("pull after remote advance: %v", err)
	}

	if got := countCommits(t, clone); got < 3 {
		t.Errorf("after pull, clone has %d commits; want >=3", got)
	}
}
