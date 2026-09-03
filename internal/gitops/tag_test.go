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

// TestCloneAtTag_PinsToTagCommit is the core guarantee of a pinned source: the
// worktree holds the tagged commit's content, not whatever the branch has
// advanced to since.
func TestCloneAtTag_PinsToTagCommit(t *testing.T) {
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
			if err := CloneAtTag(context.Background(), clone, f.url, "v1.0.0", nil); err != nil {
				t.Fatalf("CloneAtTag: %v", err)
			}
			if got := readFile(t, clone, "version"); got != "pinned" {
				t.Errorf("worktree content = %q, want %q", got, "pinned")
			}
		})
	}
}

// TestCloneAtTag_RefreshNeedsNoRemote proves the refresh of a satisfied pin
// makes no network round-trip: the remote is moved out from under the clone
// and the refresh still succeeds.
func TestCloneAtTag_RefreshNeedsNoRemote(t *testing.T) {
	f := newTagFixture(t)
	f.commit(t, "version", "pinned")
	f.tag(t, "v1.0.0", false)

	clone := filepath.Join(t.TempDir(), "clone")
	if err := CloneAtTag(context.Background(), clone, f.url, "v1.0.0", nil); err != nil {
		t.Fatalf("initial CloneAtTag: %v", err)
	}
	if err := os.RemoveAll(f.bareDir); err != nil {
		t.Fatalf("remove remote: %v", err)
	}
	if err := CloneAtTag(context.Background(), clone, f.url, "v1.0.0", nil); err != nil {
		t.Fatalf("refresh CloneAtTag reached the remote: %v", err)
	}
	if got := readFile(t, clone, "version"); got != "pinned" {
		t.Errorf("worktree content = %q, want %q", got, "pinned")
	}
}

// TestCloneAtTag_UnknownTagKeepsTheClone is the churn guard from #825: an
// unknown tag must not read as a reclonable "reference not found" and send
// the existing clone through wipe-and-re-clone against the remote.
func TestCloneAtTag_UnknownTagKeepsTheClone(t *testing.T) {
	f := newTagFixture(t)
	f.commit(t, "version", "pinned")
	f.tag(t, "v1.0.0", false)

	clone := filepath.Join(t.TempDir(), "clone")
	if err := CloneAtTag(context.Background(), clone, f.url, "v1.0.0", nil); err != nil {
		t.Fatalf("initial CloneAtTag: %v", err)
	}

	err := CloneAtTag(context.Background(), clone, f.url, "v9.9.9", nil)
	if !errors.Is(err, ErrTagNotFound) {
		t.Fatalf("CloneAtTag(unknown tag) = %v, want ErrTagNotFound", err)
	}
	if IsReclonableError(err) {
		t.Errorf("CloneAtTag(unknown tag) error %q reads as reclonable", err)
	}
	if _, statErr := os.Stat(filepath.Join(clone, ".git")); statErr != nil {
		t.Errorf("clone was wiped for an unknown tag: %v", statErr)
	}
}

// TestCloneAtTag_UnknownTagOnFirstCloneIsNamed keeps the fresh-clone path's
// failure as legible as the refresh path's — an operator who typos a tag reads
// the same error either way.
func TestCloneAtTag_UnknownTagOnFirstCloneIsNamed(t *testing.T) {
	f := newTagFixture(t)
	f.commit(t, "version", "pinned")
	f.tag(t, "v1.0.0", false)

	clone := filepath.Join(t.TempDir(), "clone")
	err := CloneAtTag(context.Background(), clone, f.url, "v9.9.9", nil)
	if !errors.Is(err, ErrTagNotFound) {
		t.Fatalf("CloneAtTag(unknown tag) = %v, want ErrTagNotFound", err)
	}
}

// TestCloneAtTag_FetchesATagAddedAfterTheClone covers the bumped-pin case
// where the clone directory is reused: the tag is absent locally and must be
// fetched by name rather than reported missing.
func TestCloneAtTag_FetchesATagAddedAfterTheClone(t *testing.T) {
	f := newTagFixture(t)
	f.commit(t, "version", "one")
	f.tag(t, "v1.0.0", false)

	clone := filepath.Join(t.TempDir(), "clone")
	if err := CloneAtTag(context.Background(), clone, f.url, "v1.0.0", nil); err != nil {
		t.Fatalf("initial CloneAtTag: %v", err)
	}

	f.commit(t, "version", "two")
	f.tag(t, "v2.0.0", false)

	if err := CloneAtTag(context.Background(), clone, f.url, "v2.0.0", nil); err != nil {
		t.Fatalf("CloneAtTag(v2.0.0): %v", err)
	}
	if got := readFile(t, clone, "version"); got != "two" {
		t.Errorf("worktree content = %q, want %q", got, "two")
	}
}
