package gitops

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// initRepoWithCommit creates a repository at root with one commit and returns
// that commit's hex ID.
func initRepoWithCommit(t *testing.T, root string) string {
	t.Helper()
	repo, err := gogit.PlainInit(root, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "task.yaml"), []byte("name: t\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if _, err := wt.Add("task.yaml"); err != nil {
		t.Fatalf("add: %v", err)
	}
	h, err := wt.Commit("seed", &gogit.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@t", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	return h.String()
}

func TestHeadCommit_FromNestedDirectory(t *testing.T) {
	root := t.TempDir()
	want := initRepoWithCommit(t, root)

	nested := filepath.Join(root, "tasks", "deploy")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := HeadCommit(nested)
	if err != nil {
		t.Fatalf("HeadCommit: %v", err)
	}
	if got != want {
		t.Fatalf("HeadCommit = %q, want %q", got, want)
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
