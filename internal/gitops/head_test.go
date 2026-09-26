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

func TestHeadInfo_TrackedNestedDirectory(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "tasks", "deploy")
	writeFile(t, filepath.Join(nested, "task.yaml"), "name: t\n")
	want := initRepo(t, root)

	got, _, err := HeadInfo(nested)
	if err != nil {
		t.Fatalf("HeadInfo: %v", err)
	}
	if got != want {
		t.Fatalf("HeadInfo = %q, want %q", got, want)
	}
}

func TestHeadInfo_RepositoryRoot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "task.yaml"), "name: t\n")
	want := initRepo(t, root)

	got, _, err := HeadInfo(root)
	if err != nil {
		t.Fatalf("HeadInfo: %v", err)
	}
	if got != want {
		t.Fatalf("HeadInfo = %q, want %q", got, want)
	}
}

// TestHeadInfo_UntrackedDirectory is the case that separates "this
// repository describes these files" from "these files merely sit inside a
// repository": tasks dropped into a version-controlled home directory would
// otherwise be stamped with a commit whose tree contains none of them.
func TestHeadInfo_UntrackedDirectory(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "unrelated.txt"), "x\n")
	initRepo(t, root)

	untracked := filepath.Join(root, "tasks", "deploy")
	writeFile(t, filepath.Join(untracked, "task.yaml"), "name: t\n")

	if got, _, err := HeadInfo(untracked); err == nil {
		t.Fatalf("HeadInfo = %q, want an error for a directory HEAD does not track", got)
	}
}

func TestHeadInfo_OutsideRepository(t *testing.T) {
	if _, _, err := HeadInfo(t.TempDir()); err == nil {
		t.Fatal("expected an error for a directory outside any repository")
	}
}

func TestHeadInfo_UnbornBranch(t *testing.T) {
	root := t.TempDir()
	if _, err := gogit.PlainInit(root, false); err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	if _, _, err := HeadInfo(root); err == nil {
		t.Fatal("expected an error for a repository with no commits")
	}
}

// TestHeadInfo_LinkedWorktree is the regression pin for #863: HeadInfo must
// resolve HEAD when dir is (or is nested inside) a linked git worktree (the
// `git worktree add` concept — a second working directory sharing one object
// store with a main checkout), not only a plain clone.
//
// go-git has no API to create a linked worktree, so this hand-builds the
// on-disk layout git itself produces, following go-git's own
// PlainOpenWithOptions/EnableDotGitCommonDir implementation
// (repository.go's dotGitToOSFilesystems/dotGitCommonDirectory) rather than
// guessing:
//
//   - <main>/.git/worktrees/<name>/HEAD — the worktree's own checked-out ref
//     (here a detached commit hash, git's own format for a HEAD not on a
//     branch). go-git's RepositoryFilesystem routes top-level "HEAD" to this
//     per-worktree directory rather than the common one (see
//     storage/filesystem/dotgit/repository_filesystem.go's
//     mapToRepositoryFsByPath: "HEAD" is not among the objects/refs/config/…
//     paths that always route to commondir), exactly like a real worktree.
//   - <main>/.git/worktrees/<name>/commondir — "../..", the relative path
//     git itself writes there, back to <main>/.git where the object database
//     and refs actually live.
//   - <linked>/.git — a *file* (not a directory), containing
//     "gitdir: <absolute path to <main>/.git/worktrees/<name>>". This is
//     what tells go-git's dotGitToOSFilesystems that dir belongs to that
//     per-worktree directory in the first place.
//
// A "gitdir" reverse-pointer file inside worktrees/<name>/ (real git writes
// one back at the linked checkout) is deliberately omitted: reading
// go-git's source confirms PlainOpenWithOptions never opens such a file —
// only "commondir" and "HEAD" are read from that directory — so adding it
// would only decorate the fixture, not exercise anything.
func TestHeadInfo_LinkedWorktree(t *testing.T) {
	root := t.TempDir()
	mainRepo := filepath.Join(root, "main")
	nested := filepath.Join(mainRepo, "tasks", "deploy")
	writeFile(t, filepath.Join(nested, "task.yaml"), "name: t\n")
	want := initRepo(t, mainRepo)

	const worktreeName = "wt1"
	worktreeGitDir := filepath.Join(mainRepo, ".git", "worktrees", worktreeName)
	writeFile(t, filepath.Join(worktreeGitDir, "HEAD"), want+"\n")
	writeFile(t, filepath.Join(worktreeGitDir, "commondir"), "../..\n")

	linked := filepath.Join(root, "linked")
	writeFile(t, filepath.Join(linked, ".git"), "gitdir: "+worktreeGitDir+"\n")
	// The linked worktree's checkout of the same tracked path — HeadInfo
	// only needs it to exist on disk for filepath.Rel/DetectDotGit to walk
	// up to the .git file; the tree lookup itself reads git objects, not
	// this file's content.
	linkedNested := filepath.Join(linked, "tasks", "deploy")
	writeFile(t, filepath.Join(linkedNested, "task.yaml"), "name: t\n")

	got, _, err := HeadInfo(linkedNested)
	if err != nil {
		t.Fatalf("HeadInfo: %v", err)
	}
	if got != want {
		t.Fatalf("HeadInfo = %q, want %q", got, want)
	}
}
