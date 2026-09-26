package gitops

import (
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
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

// corruptTreeCommit builds and stores, directly against root's repository
// object store, a commit whose root tree contains one entry — "subdir" — of
// mode Dir pointing at a tree hash that was never stored. Walking FindEntry
// into "subdir/..." therefore fails not with object.ErrEntryNotFound or
// object.ErrDirectoryNotFound (go-git's ordinary "not present" signal), but
// with the object store's raw plumbing.ErrObjectNotFound raised
// while trying to load the subtree itself — go-git's Tree.dir only
// translates a *lookup* miss to ErrDirectoryNotFound; a decode/read failure
// on an entry that IS present propagates unclassified. That is the
// "repository fault, not an absent path" case
// TreeBlobHashesForPathsAtTwoCommits must surface as a whole-call error
// rather than silently reading as "absent".
func corruptTreeCommit(t *testing.T, root string) string {
	t.Helper()
	repo, err := gogit.PlainOpen(root)
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}

	bogus := plumbing.NewHash("dddddddddddddddddddddddddddddddddddddddd")
	tree := &object.Tree{
		Entries: []object.TreeEntry{
			{Name: "subdir", Mode: filemode.Dir, Hash: bogus},
		},
	}
	treeObj := repo.Storer.NewEncodedObject()
	treeObj.SetType(plumbing.TreeObject)
	if err := tree.Encode(treeObj); err != nil {
		t.Fatalf("encode corrupt tree: %v", err)
	}
	treeHash, err := repo.Storer.SetEncodedObject(treeObj)
	if err != nil {
		t.Fatalf("store corrupt tree: %v", err)
	}

	now := time.Now()
	commit := &object.Commit{
		Author:    object.Signature{Name: "t", Email: "t@t", When: now},
		Committer: object.Signature{Name: "t", Email: "t@t", When: now},
		Message:   "corrupt",
		TreeHash:  treeHash,
	}
	commitObj := repo.Storer.NewEncodedObject()
	commitObj.SetType(plumbing.CommitObject)
	if err := commit.Encode(commitObj); err != nil {
		t.Fatalf("encode corrupt commit: %v", err)
	}
	commitHash, err := repo.Storer.SetEncodedObject(commitObj)
	if err != nil {
		t.Fatalf("store corrupt commit: %v", err)
	}
	return commitHash.String()
}

// TestTreeBlobHashesForPathsAtTwoCommits_UnchangedFileMatches pins the
// "unchanged" case: a file untouched between two commits resolves to the
// same blob hash in both trees, and both trees are resolved correctly by a
// single call.
func TestTreeBlobHashesForPathsAtTwoCommits_UnchangedFileMatches(t *testing.T) {
	root := t.TempDir()
	stable := filepath.Join(root, "task.yaml")
	writeFile(t, stable, "name: t\n")
	first := initRepo(t, root)

	// A second commit that doesn't touch stable.
	writeFile(t, filepath.Join(root, "other.txt"), "x\n")
	second := commitChange(t, root, "add other")

	fromHashes, toHashes, err := TreeBlobHashesForPathsAtTwoCommits(root, first, second, []string{stable})
	if err != nil {
		t.Fatalf("TreeBlobHashesForPathsAtTwoCommits: %v", err)
	}
	if fromHashes[stable] == "" || toHashes[stable] == "" {
		t.Fatalf("expected a blob hash in both trees, got %q and %q", fromHashes[stable], toHashes[stable])
	}
	if fromHashes[stable] != toHashes[stable] {
		t.Errorf("unchanged file: hash differs across commits: %q vs %q", fromHashes[stable], toHashes[stable])
	}
}

// TestTreeBlobHashesForPathsAtTwoCommits_ChangedFileDiffers pins the
// "changed" case: editing a file's content between two commits changes the
// blob hash its path resolves to, for both commits resolved in one call.
func TestTreeBlobHashesForPathsAtTwoCommits_ChangedFileDiffers(t *testing.T) {
	root := t.TempDir()
	edited := filepath.Join(root, "task.js")
	writeFile(t, edited, "export default () => 1;\n")
	first := initRepo(t, root)

	writeFile(t, edited, "export default () => 2;\n")
	second := commitChange(t, root, "edit task.js")

	fromHashes, toHashes, err := TreeBlobHashesForPathsAtTwoCommits(root, first, second, []string{edited})
	if err != nil {
		t.Fatalf("TreeBlobHashesForPathsAtTwoCommits: %v", err)
	}
	if fromHashes[edited] == "" || toHashes[edited] == "" {
		t.Fatalf("expected a blob hash in both trees, got %q and %q", fromHashes[edited], toHashes[edited])
	}
	if fromHashes[edited] == toHashes[edited] {
		t.Errorf("changed file: hash identical across commits: %q", fromHashes[edited])
	}
}

// TestTreeBlobHashesForPathsAtTwoCommits_NewFileAbsentFromEarlierTree pins
// the "new" case: a file added at the later commit resolves in that
// commit's tree but is simply absent (no error) from the earlier one's —
// both read from the one call's two result maps.
func TestTreeBlobHashesForPathsAtTwoCommits_NewFileAbsentFromEarlierTree(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "task.yaml"), "name: t\n")
	first := initRepo(t, root)

	added := filepath.Join(root, "new.js")
	writeFile(t, added, "export default () => {};\n")
	second := commitChange(t, root, "add new.js")

	fromHashes, toHashes, err := TreeBlobHashesForPathsAtTwoCommits(root, first, second, []string{added})
	if err != nil {
		t.Fatalf("TreeBlobHashesForPathsAtTwoCommits: %v", err)
	}
	if _, ok := fromHashes[added]; ok {
		t.Errorf("new.js must be absent from the commit before it was added, got %q", fromHashes[added])
	}
	if toHashes[added] == "" {
		t.Error("new.js must resolve to a blob hash in the commit that added it")
	}
}

// TestTreeBlobHashesForPathsAtTwoCommits_PathOutsideRepoRootIsAbsentNotError
// pins that a path outside the repository (e.g. resolved before a task
// moved, or a hash_include target the caller mistakenly passed from a
// different repo) degrades to "absent from both results", never a
// whole-call error.
func TestTreeBlobHashesForPathsAtTwoCommits_PathOutsideRepoRootIsAbsentNotError(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "task.yaml"), "name: t\n")
	first := initRepo(t, root)
	writeFile(t, filepath.Join(root, "other.txt"), "x\n")
	second := commitChange(t, root, "add other")

	outside := filepath.Join(t.TempDir(), "elsewhere.txt")

	fromHashes, toHashes, err := TreeBlobHashesForPathsAtTwoCommits(root, first, second, []string{outside})
	if err != nil {
		t.Fatalf("TreeBlobHashesForPathsAtTwoCommits: %v", err)
	}
	if _, ok := fromHashes[outside]; ok {
		t.Errorf("path outside the repository root must be absent from fromHashes, got %q", fromHashes[outside])
	}
	if _, ok := toHashes[outside]; ok {
		t.Errorf("path outside the repository root must be absent from toHashes, got %q", toHashes[outside])
	}
}

// TestTreeBlobHashesForPathsAtTwoCommits_UnresolvableShaIsAnError pins one
// of the whole-call error cases: there is no baseline of any shape to
// compare against, whichever side of the pair fails to resolve.
func TestTreeBlobHashesForPathsAtTwoCommits_UnresolvableShaIsAnError(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "task.yaml"), "name: t\n")
	first := initRepo(t, root)
	zero := "0000000000000000000000000000000000000000"

	if _, _, err := TreeBlobHashesForPathsAtTwoCommits(root, zero, first, []string{filepath.Join(root, "task.yaml")}); err == nil {
		t.Fatal("expected an error for a fromSHA with no corresponding commit")
	}
	if _, _, err := TreeBlobHashesForPathsAtTwoCommits(root, first, zero, []string{filepath.Join(root, "task.yaml")}); err == nil {
		t.Fatal("expected an error for a toSHA with no corresponding commit")
	}
}

// TestTreeBlobHashesForPathsAtTwoCommits_OutsideRepositoryIsAnError pins the
// other whole-call error case: dir names no repository at all.
func TestTreeBlobHashesForPathsAtTwoCommits_OutsideRepositoryIsAnError(t *testing.T) {
	zero := "0000000000000000000000000000000000000000"
	if _, _, err := TreeBlobHashesForPathsAtTwoCommits(t.TempDir(), zero, zero, nil); err == nil {
		t.Fatal("expected an error for a directory outside any repository")
	}
}

// TestTreeBlobHashesForPathsAtTwoCommits_NotFoundStillDegradesToAbsent is
// the regression pin for Finding 1: go-git's two genuine "not present in
// this tree" errors (object.ErrEntryNotFound, object.ErrDirectoryNotFound)
// must still degrade a path to "absent from the result" after the error
// reclassification — only a NON-not-found error may surface. This exercises
// both a missing leaf entry and a missing intermediate directory component,
// each against a tree that otherwise resolves fine.
func TestTreeBlobHashesForPathsAtTwoCommits_NotFoundStillDegradesToAbsent(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "task.yaml"), "name: t\n")
	writeFile(t, filepath.Join(root, "sub", "present.js"), "export default 1;\n")
	sha := initRepo(t, root)

	missingLeaf := filepath.Join(root, "sub", "absent.js")
	missingDir := filepath.Join(root, "nosuchdir", "absent.js")

	fromHashes, toHashes, err := TreeBlobHashesForPathsAtTwoCommits(root, sha, sha, []string{missingLeaf, missingDir})
	if err != nil {
		t.Fatalf("TreeBlobHashesForPathsAtTwoCommits: unexpected whole-call error for genuinely absent paths: %v", err)
	}
	for _, hashes := range []map[string]string{fromHashes, toHashes} {
		if _, ok := hashes[missingLeaf]; ok {
			t.Errorf("missing leaf entry must be absent, got %q", hashes[missingLeaf])
		}
		if _, ok := hashes[missingDir]; ok {
			t.Errorf("missing intermediate directory must be absent, got %q", hashes[missingDir])
		}
	}
}

// TestTreeBlobHashesForPathsAtTwoCommits_NonNotFoundTreeErrorSurfaces is the
// Finding 1 regression pin for the other side of the reclassification: a
// per-path tree-read failure that is NOT one of go-git's two "not present"
// errors — here, an entry that exists but whose target tree object was
// never stored, simulating pack corruption / a transient object-read fault
// — must surface as a whole-call error rather than silently reading as
// "absent" (which would render identically to "unchanged" on the approval
// review surface and violate ADR-0001's "never a false claim" invariant).
func TestTreeBlobHashesForPathsAtTwoCommits_NonNotFoundTreeErrorSurfaces(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "task.yaml"), "name: t\n")
	good := initRepo(t, root)
	bad := corruptTreeCommit(t, root)

	target := filepath.Join(root, "subdir", "foo.js")

	if _, _, err := TreeBlobHashesForPathsAtTwoCommits(root, good, bad, []string{target}); err == nil {
		t.Fatal("expected a whole-call error when a per-path tree lookup fails with something other than not-found")
	}
	if _, _, err := TreeBlobHashesForPathsAtTwoCommits(root, bad, good, []string{target}); err == nil {
		t.Fatal("expected a whole-call error regardless of which side (from/to) hits the corrupt tree")
	}
}
