package gitops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// tagFixture is a bare repo plus the worktree used to seed it.
type tagFixture struct {
	bareDir string
	url     string
	wt      *gogit.Repository
	wtPath  string
}

func newTagFixture(t *testing.T) *tagFixture {
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
	if _, err := wt.CreateRemote(&gogitconfig.RemoteConfig{Name: "origin", URLs: []string{bareDir}}); err != nil {
		t.Fatalf("create remote: %v", err)
	}
	return &tagFixture{bareDir: bareDir, url: TestFixtureRemoteURL(bareDir), wt: wt, wtPath: wtPath}
}

func (f *tagFixture) commit(t *testing.T, filename, body string) plumbing.Hash {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.wtPath, filename), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	tree, err := f.wt.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if _, err := tree.Add(filename); err != nil {
		t.Fatalf("add: %v", err)
	}
	h, err := tree.Commit("seed "+filename, &gogit.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@t", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	f.push(t)
	return h
}

// tag creates a tag at the current HEAD. annotated selects between a
// lightweight ref and a real tag object, which resolve differently: the
// annotated form points at a tag object that must be peeled to reach a commit.
func (f *tagFixture) tag(t *testing.T, name string, annotated bool) {
	t.Helper()
	head, err := f.wt.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	var opts *gogit.CreateTagOptions
	if annotated {
		opts = &gogit.CreateTagOptions{
			Message: name,
			Tagger:  &object.Signature{Name: "t", Email: "t@t", When: time.Now()},
		}
	}
	if _, err := f.wt.CreateTag(name, head.Hash(), opts); err != nil {
		t.Fatalf("create tag %s: %v", name, err)
	}
	f.push(t)
}

// retag moves an existing tag onto the current HEAD, as a re-cut release does.
func (f *tagFixture) retag(t *testing.T, name string) {
	t.Helper()
	if err := f.wt.DeleteTag(name); err != nil {
		t.Fatalf("delete tag %s: %v", name, err)
	}
	head, err := f.wt.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if _, err := f.wt.CreateTag(name, head.Hash(), nil); err != nil {
		t.Fatalf("re-create tag %s: %v", name, err)
	}
	f.push(t)
}

// resetBranchTo rewinds branch by n commits and force-pushes it, as a
// history rewrite on the remote does.
func (f *tagFixture) resetBranchTo(t *testing.T, branch string, n int) {
	t.Helper()
	head, err := f.wt.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	commit, err := f.wt.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("commit object: %v", err)
	}
	for i := 0; i < n; i++ {
		if commit, err = commit.Parent(0); err != nil {
			t.Fatalf("parent %d: %v", i, err)
		}
	}
	tree, err := f.wt.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if err := tree.Reset(&gogit.ResetOptions{Mode: gogit.HardReset, Commit: commit.Hash}); err != nil {
		t.Fatalf("reset seed worktree: %v", err)
	}
	f.push(t)
}

func (f *tagFixture) push(t *testing.T) {
	t.Helper()
	err := f.wt.Push(&gogit.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []gogitconfig.RefSpec{"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"},
	})
	if err != nil && err != gogit.NoErrAlreadyUpToDate {
		t.Fatalf("push: %v", err)
	}
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func branchRef(name string) plumbing.ReferenceName { return plumbing.NewBranchReferenceName(name) }
func tagRef(name string) plumbing.ReferenceName    { return plumbing.NewTagReferenceName(name) }

// TestCloneAtRef_ChecksOutTheNamedRef is the core guarantee: the worktree
// holds the named ref's content, and a tag is not confused with the branch it
// was cut from.
func TestCloneAtRef_PinsToTagCommit(t *testing.T) {
	for _, annotated := range []bool{false, true} {
		name := "lightweight"
		if annotated {
			name = "annotated"
		}
		t.Run(name, func(t *testing.T) {
			f := newTagFixture(t)
			f.commit(t, "version", "pinned")
			f.tag(t, "v1.0.0", annotated)
			f.commit(t, "version", "moved on")

			clone := filepath.Join(t.TempDir(), "clone")
			if err := CloneAtRef(context.Background(), clone, f.url, tagRef("v1.0.0"), nil); err != nil {
				t.Fatalf("CloneAtRef: %v", err)
			}
			if got := readFile(t, clone, "version"); got != "pinned" {
				t.Errorf("worktree content = %q, want %q", got, "pinned")
			}
		})
	}
}

// TestCloneAtRef_FollowsAMovedRef covers the deliberate decision that a
// re-pointed ref is followed rather than pinned to what the clone first saw.
// Not following would have been enforced only by the clone cache surviving,
// so two daemons on one config could silently run different commits; the
// approval gate re-pends the changed content instead.
func TestCloneAtRef_FollowsAMovedRef(t *testing.T) {
	for _, tc := range []struct {
		name string
		ref  plumbing.ReferenceName
		move func(t *testing.T, f *tagFixture)
	}{
		{
			name: "re-cut tag",
			ref:  tagRef("v1.0.0"),
			move: func(t *testing.T, f *tagFixture) {
				f.commit(t, "version", "two")
				f.retag(t, "v1.0.0")
			},
		},
		{
			name: "branch advance",
			ref:  branchRef("main"),
			move: func(t *testing.T, f *tagFixture) { f.commit(t, "version", "two") },
		},
		{
			name: "rewound branch",
			ref:  branchRef("main"),
			move: func(t *testing.T, f *tagFixture) {
				f.commit(t, "version", "two")
				f.commit(t, "version", "three")
				f.resetBranchTo(t, "main", 2) // drop the last two commits
				f.commit(t, "version", "two")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newTagFixture(t)
			f.commit(t, "version", "one")
			f.tag(t, "v1.0.0", false)

			clone := filepath.Join(t.TempDir(), "clone")
			if err := CloneAtRef(context.Background(), clone, f.url, tc.ref, nil); err != nil {
				t.Fatalf("initial CloneAtRef: %v", err)
			}
			if got := readFile(t, clone, "version"); got != "one" {
				t.Fatalf("worktree content = %q, want %q", got, "one")
			}

			tc.move(t, f)

			if err := CloneAtRef(context.Background(), clone, f.url, tc.ref, nil); err != nil {
				t.Fatalf("refresh CloneAtRef: %v", err)
			}
			if got := readFile(t, clone, "version"); got != "two" {
				t.Errorf("worktree content = %q after the ref moved, want %q", got, "two")
			}
		})
	}
}

// TestCloneAtRef_UnmovedRefLeavesTheWorktreeAlone is load-bearing rather than
// an optimization: pkg/taskset's watch loop only re-resolves when fsnotify
// reports a change, so a refresh that rewrote every file on each poll tick
// would drive a re-resolve every tick for every source.
func TestCloneAtRef_UnmovedRefLeavesTheWorktreeAlone(t *testing.T) {
	f := newTagFixture(t)
	f.commit(t, "version", "one")

	clone := filepath.Join(t.TempDir(), "clone")
	if err := CloneAtRef(context.Background(), clone, f.url, branchRef("main"), nil); err != nil {
		t.Fatalf("initial CloneAtRef: %v", err)
	}
	tracked := filepath.Join(clone, "version")
	before, err := os.Stat(tracked)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if err := CloneAtRef(context.Background(), clone, f.url, branchRef("main"), nil); err != nil {
		t.Fatalf("refresh CloneAtRef: %v", err)
	}
	after, err := os.Stat(tracked)
	if err != nil {
		t.Fatalf("stat after refresh: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("refresh rewrote an unchanged file (mtime %v → %v); fsnotify would fire on every poll tick",
			before.ModTime(), after.ModTime())
	}
}

// TestCloneAtRef_UnknownTagKeepsTheClone is the churn guard from #825: an
// unknown tag must not read as a reclonable "reference not found" and send
// the existing clone through wipe-and-re-clone against the remote.
func TestCloneAtRef_UnknownTagKeepsTheClone(t *testing.T) {
	f := newTagFixture(t)
	f.commit(t, "version", "pinned")
	f.tag(t, "v1.0.0", false)

	clone := filepath.Join(t.TempDir(), "clone")
	if err := CloneAtRef(context.Background(), clone, f.url, tagRef("v1.0.0"), nil); err != nil {
		t.Fatalf("initial CloneAtRef: %v", err)
	}

	err := CloneAtRef(context.Background(), clone, f.url, tagRef("v9.9.9"), nil)
	if !errors.Is(err, ErrRefNotFound) {
		t.Fatalf("CloneAtRef(unknown tag) = %v, want ErrRefNotFound", err)
	}
	if IsReclonableError(err) {
		t.Errorf("CloneAtRef(unknown tag) error %q reads as reclonable", err)
	}
	if _, statErr := os.Stat(filepath.Join(clone, ".git")); statErr != nil {
		t.Errorf("clone was wiped for an unknown tag: %v", statErr)
	}
}

// TestCloneAtRef_UnknownTagOnFirstCloneIsNamed keeps the fresh-clone path's
// failure as legible as the refresh path's — an operator who typos a tag reads
// the same error either way.
func TestCloneAtRef_UnknownTagOnFirstCloneIsNamed(t *testing.T) {
	f := newTagFixture(t)
	f.commit(t, "version", "pinned")
	f.tag(t, "v1.0.0", false)

	clone := filepath.Join(t.TempDir(), "clone")
	err := CloneAtRef(context.Background(), clone, f.url, tagRef("v9.9.9"), nil)
	if !errors.Is(err, ErrRefNotFound) {
		t.Fatalf("CloneAtRef(unknown tag) = %v, want ErrRefNotFound", err)
	}
}

// TestCloneAtRef_FetchesATagAddedAfterTheClone covers the bumped-pin case
// where the clone directory is reused: the tag is absent locally and must be
// fetched by name rather than reported missing.
func TestCloneAtRef_FetchesATagAddedAfterTheClone(t *testing.T) {
	f := newTagFixture(t)
	f.commit(t, "version", "one")
	f.tag(t, "v1.0.0", false)

	clone := filepath.Join(t.TempDir(), "clone")
	if err := CloneAtRef(context.Background(), clone, f.url, tagRef("v1.0.0"), nil); err != nil {
		t.Fatalf("initial CloneAtRef: %v", err)
	}

	f.commit(t, "version", "two")
	f.tag(t, "v2.0.0", false)

	if err := CloneAtRef(context.Background(), clone, f.url, tagRef("v2.0.0"), nil); err != nil {
		t.Fatalf("CloneAtRef(v2.0.0): %v", err)
	}
	if got := readFile(t, clone, "version"); got != "two" {
		t.Errorf("worktree content = %q, want %q", got, "two")
	}
}
