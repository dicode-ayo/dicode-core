package gitops

import (
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// commitChange stages every path already written under root and commits
// them, returning the resulting commit ID. Shared with initRepo's "seed"
// commit (head_test.go) via the same repository, so the two commits share
// history — exactly the shape a re-pending task produces (#670).
func commitChange(t *testing.T, root, msg string) string {
	t.Helper()
	repo, err := gogit.PlainOpen(root)
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("add: %v", err)
	}
	h, err := wt.Commit(msg, &gogit.CommitOptions{
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

// TestTreeBlobHashesForPaths_UnchangedFileMatchesAcrossCommits pins the
// "unchanged" case: a file untouched between two commits resolves to the
// same blob hash in both trees.
func TestTreeBlobHashesForPaths_UnchangedFileMatchesAcrossCommits(t *testing.T) {
	root := t.TempDir()
	stable := filepath.Join(root, "task.yaml")
	writeFile(t, stable, "name: t\n")
	first := initRepo(t, root)

	// A second commit that doesn't touch stable.
	writeFile(t, filepath.Join(root, "other.txt"), "x\n")
	second := commitChange(t, root, "add other")

	atFirst, err := TreeBlobHashesForPaths(root, first, []string{stable})
	if err != nil {
		t.Fatalf("TreeBlobHashesForPaths(first): %v", err)
	}
	atSecond, err := TreeBlobHashesForPaths(root, second, []string{stable})
	if err != nil {
		t.Fatalf("TreeBlobHashesForPaths(second): %v", err)
	}
	if atFirst[stable] == "" || atSecond[stable] == "" {
		t.Fatalf("expected a blob hash in both trees, got %q and %q", atFirst[stable], atSecond[stable])
	}
	if atFirst[stable] != atSecond[stable] {
		t.Errorf("unchanged file: hash differs across commits: %q vs %q", atFirst[stable], atSecond[stable])
	}
}

// TestTreeBlobHashesForPaths_ChangedFileDiffersAcrossCommits pins the
// "changed" case: editing a file's content between two commits changes the
// blob hash its path resolves to.
func TestTreeBlobHashesForPaths_ChangedFileDiffersAcrossCommits(t *testing.T) {
	root := t.TempDir()
	edited := filepath.Join(root, "task.js")
	writeFile(t, edited, "export default () => 1;\n")
	first := initRepo(t, root)

	writeFile(t, edited, "export default () => 2;\n")
	second := commitChange(t, root, "edit task.js")

	atFirst, err := TreeBlobHashesForPaths(root, first, []string{edited})
	if err != nil {
		t.Fatalf("TreeBlobHashesForPaths(first): %v", err)
	}
	atSecond, err := TreeBlobHashesForPaths(root, second, []string{edited})
	if err != nil {
		t.Fatalf("TreeBlobHashesForPaths(second): %v", err)
	}
	if atFirst[edited] == "" || atSecond[edited] == "" {
		t.Fatalf("expected a blob hash in both trees, got %q and %q", atFirst[edited], atSecond[edited])
	}
	if atFirst[edited] == atSecond[edited] {
		t.Errorf("changed file: hash identical across commits: %q", atFirst[edited])
	}
}

// TestTreeBlobHashesForPaths_NewFileAbsentFromEarlierTree pins the "new"
// case: a file added at the later commit resolves in that commit's tree but
// is simply absent (no error) from the earlier one's.
func TestTreeBlobHashesForPaths_NewFileAbsentFromEarlierTree(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "task.yaml"), "name: t\n")
	first := initRepo(t, root)

	added := filepath.Join(root, "new.js")
	writeFile(t, added, "export default () => {};\n")
	second := commitChange(t, root, "add new.js")

	atFirst, err := TreeBlobHashesForPaths(root, first, []string{added})
	if err != nil {
		t.Fatalf("TreeBlobHashesForPaths(first): %v", err)
	}
	if _, ok := atFirst[added]; ok {
		t.Errorf("new.js must be absent from the commit before it was added, got %q", atFirst[added])
	}

	atSecond, err := TreeBlobHashesForPaths(root, second, []string{added})
	if err != nil {
		t.Fatalf("TreeBlobHashesForPaths(second): %v", err)
	}
	if atSecond[added] == "" {
		t.Error("new.js must resolve to a blob hash in the commit that added it")
	}
}

// TestTreeBlobHashesForPaths_PathOutsideRepoRootIsAbsentNotError pins that a
// path outside the repository (e.g. resolved before a task moved, or a
// hash_include target the caller mistakenly passed from a different repo)
// degrades to "absent from the result", never a whole-call error.
func TestTreeBlobHashesForPaths_PathOutsideRepoRootIsAbsentNotError(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "task.yaml"), "name: t\n")
	first := initRepo(t, root)

	outside := filepath.Join(t.TempDir(), "elsewhere.txt")

	got, err := TreeBlobHashesForPaths(root, first, []string{outside})
	if err != nil {
		t.Fatalf("TreeBlobHashesForPaths: %v", err)
	}
	if _, ok := got[outside]; ok {
		t.Errorf("path outside the repository root must be absent from the result, got %q", got[outside])
	}
}

// TestTreeBlobHashesForPaths_UnresolvableShaIsAnError pins the one case that
// does surface as a whole-call error: there is no baseline of any shape to
// compare against.
func TestTreeBlobHashesForPaths_UnresolvableShaIsAnError(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "task.yaml"), "name: t\n")
	initRepo(t, root)

	if _, err := TreeBlobHashesForPaths(root, "0000000000000000000000000000000000000000", []string{filepath.Join(root, "task.yaml")}); err == nil {
		t.Fatal("expected an error for a sha with no corresponding commit")
	}
}

// TestTreeBlobHashesForPaths_OutsideRepositoryIsAnError pins the other
// whole-call error case: dir names no repository at all.
func TestTreeBlobHashesForPaths_OutsideRepositoryIsAnError(t *testing.T) {
	if _, err := TreeBlobHashesForPaths(t.TempDir(), "0000000000000000000000000000000000000000", nil); err == nil {
		t.Fatal("expected an error for a directory outside any repository")
	}
}
