package gitops

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// writeFile creates path and its parents.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// initRepo creates a repository at root and commits everything already under
// it, returning the resulting commit ID.
func initRepo(t *testing.T, root string) string {
	t.Helper()
	repo, err := gogit.PlainInit(root, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("add: %v", err)
	}
	h, err := wt.Commit("seed", &gogit.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@t", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if h.IsZero() {
		t.Fatal("fixture produced no commit")
	}
	return h.String()
}

func TestHeadCommit_TrackedNestedDirectory(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "tasks", "deploy")
	writeFile(t, filepath.Join(nested, "task.yaml"), "name: t\n")
	want := initRepo(t, root)

	got, err := HeadCommit(nested)
	if err != nil {
		t.Fatalf("HeadCommit: %v", err)
	}
	if got != want {
		t.Fatalf("HeadCommit = %q, want %q", got, want)
	}
}

func TestHeadCommit_RepositoryRoot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "task.yaml"), "name: t\n")
	want := initRepo(t, root)

	got, err := HeadCommit(root)
	if err != nil {
		t.Fatalf("HeadCommit: %v", err)
	}
	if got != want {
		t.Fatalf("HeadCommit = %q, want %q", got, want)
	}
}

// TestHeadCommit_UntrackedDirectory is the case that separates "this
// repository describes these files" from "these files merely sit inside a
// repository": tasks dropped into a version-controlled home directory would
// otherwise be stamped with a commit whose tree contains none of them.
func TestHeadCommit_UntrackedDirectory(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "unrelated.txt"), "x\n")
	initRepo(t, root)

	untracked := filepath.Join(root, "tasks", "deploy")
	writeFile(t, filepath.Join(untracked, "task.yaml"), "name: t\n")

	if got, err := HeadCommit(untracked); err == nil {
		t.Fatalf("HeadCommit = %q, want an error for a directory HEAD does not track", got)
	}
}

func TestHeadCommit_OutsideRepository(t *testing.T) {
	if _, err := HeadCommit(t.TempDir()); err == nil {
		t.Fatal("expected an error for a directory outside any repository")
	}
}

func TestHeadCommit_UnbornBranch(t *testing.T) {
	root := t.TempDir()
	if _, err := gogit.PlainInit(root, false); err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	if _, err := HeadCommit(root); err == nil {
		t.Fatal("expected an error for a repository with no commits")
	}
}
