package gitops

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// seedInitRepo initializes a repo at root with one committed file, returning
// the seed commit's ID.
func seedInitRepo(t *testing.T, root string) string {
	t.Helper()
	writeFile(t, filepath.Join(root, "f.txt"), "0")
	return initRepo(t, root)
}

// nextChange overwrites root/f.txt with new content and commits it, so each
// call produces a genuine, distinct commit rather than tripping go-git's
// "clean working tree" refusal on a no-op commit.
func nextChange(t *testing.T, root string, n int) string {
	t.Helper()
	writeFile(t, filepath.Join(root, "f.txt"), fmt.Sprintf("%d", n))
	return commitChange(t, root, fmt.Sprintf("change %d", n))
}

// commitWithParents overwrites root/f.txt with distinct content, stages it,
// and commits with an explicit parent list — bypassing whatever HEAD would
// otherwise supply — so a test can build a merge commit or a side-branch
// commit that Worktree.Commit's default single-parent behavior cannot.
func commitWithParents(t *testing.T, root, msg string, n int, parents []plumbing.Hash) string {
	t.Helper()
	writeFile(t, filepath.Join(root, "f.txt"), fmt.Sprintf("%d", n))
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
		Author:  &object.Signature{Name: "t", Email: "t@t", When: time.Now()},
		Parents: parents,
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	return h.String()
}

// TestCommitCountBetween_MergeCommitCountsFirstParentOnly builds a mainline
// with a merge commit pulling in an independent side branch, and pins that
// the walk follows the merge commit's first parent (the mainline) and never
// the side branch: the count must reflect only the commits that landed on
// the tracked branch itself, not every commit reachable through the merge.
func TestCommitCountBetween_MergeCommitCountsFirstParentOnly(t *testing.T) {
	root := t.TempDir()
	from := seedInitRepo(t, root)
	fromHash := plumbing.NewHash(from)

	nextChange(t, root, 1)
	m2 := nextChange(t, root, 2)

	side := commitWithParents(t, root, "side branch commit", 100, []plumbing.Hash{fromHash})

	merge := commitWithParents(t, root, "merge side into mainline", 200,
		[]plumbing.Hash{plumbing.NewHash(m2), plumbing.NewHash(side)})

	// First-parent chain from merge: merge -> m2 -> m1 -> from. Three
	// commits after from, none of them the side-branch commit.
	count, bounded, err := CommitCountBetween(root, from, merge, 500)
	if err != nil {
		t.Fatalf("CommitCountBetween: %v", err)
	}
	if count != 3 || bounded {
		t.Errorf("got (%d, %v), want (3, false) — merge, m2, m1 only, never the side-branch commit", count, bounded)
	}
}

// TestCommitCountBetween_FromOnlyReachableViaSideBranchIsAnError covers the
// other half of first-parent-only semantics: a fromSHA that the merge pulled
// in through its *second* parent, and that the first-parent chain never
// passes through, must not be silently reported as some count — there is no
// well-defined "number of commits" for a baseline the tracked branch's own
// history doesn't contain.
func TestCommitCountBetween_FromOnlyReachableViaSideBranchIsAnError(t *testing.T) {
	root := t.TempDir()
	from := seedInitRepo(t, root)
	fromHash := plumbing.NewHash(from)

	nextChange(t, root, 1)
	m2 := nextChange(t, root, 2)
	side := commitWithParents(t, root, "side branch commit", 100, []plumbing.Hash{fromHash})
	merge := commitWithParents(t, root, "merge side into mainline", 200,
		[]plumbing.Hash{plumbing.NewHash(m2), plumbing.NewHash(side)})

	_, _, err := CommitCountBetween(root, side, merge, 500)
	if err == nil {
		t.Fatal("CommitCountBetween: want error when fromSHA is reachable only via a non-first parent, got nil")
	}
}

func TestCommitCountBetween_SameCommitIsZero(t *testing.T) {
	root := t.TempDir()
	head := seedInitRepo(t, root)

	count, bounded, err := CommitCountBetween(root, head, head, 500)
	if err != nil {
		t.Fatalf("CommitCountBetween: %v", err)
	}
	if count != 0 || bounded {
		t.Errorf("got (%d, %v), want (0, false)", count, bounded)
	}
}

func TestCommitCountBetween_ExactCount(t *testing.T) {
	root := t.TempDir()
	from := seedInitRepo(t, root)
	nextChange(t, root, 1)
	nextChange(t, root, 2)
	to := nextChange(t, root, 3)

	count, bounded, err := CommitCountBetween(root, from, to, 500)
	if err != nil {
		t.Fatalf("CommitCountBetween: %v", err)
	}
	if count != 3 || bounded {
		t.Errorf("got (%d, %v), want (3, false)", count, bounded)
	}
}

func TestCommitCountBetween_BoundedAtLimit(t *testing.T) {
	root := t.TempDir()
	from := seedInitRepo(t, root)
	for i := 1; i <= 5; i++ {
		nextChange(t, root, i)
	}
	to := nextChange(t, root, 6)

	count, bounded, err := CommitCountBetween(root, from, to, 3)
	if err != nil {
		t.Fatalf("CommitCountBetween: %v", err)
	}
	if count != 3 || !bounded {
		t.Errorf("got (%d, %v), want (3, true) — the walk must stop at the cap", count, bounded)
	}
}

// TestCommitCountBetween_ExactCountAtLimitIsNotBounded covers the boundary
// CodeRabbit flagged on PR #890: a true distance exactly equal to limit must
// report an exact count (bounded=false), not "N+" — the walk must confirm
// whether the very next commit is fromSHA before giving up at the cap.
func TestCommitCountBetween_ExactCountAtLimitIsNotBounded(t *testing.T) {
	root := t.TempDir()
	from := seedInitRepo(t, root)
	nextChange(t, root, 1)
	nextChange(t, root, 2)
	to := nextChange(t, root, 3)

	count, bounded, err := CommitCountBetween(root, from, to, 3)
	if err != nil {
		t.Fatalf("CommitCountBetween: %v", err)
	}
	if count != 3 || bounded {
		t.Errorf("got (%d, %v), want (3, false) — true distance equals limit exactly, so the count is exact", count, bounded)
	}
}

// TestCommitCountBetween_FromAbsentFromRepoIsAnError covers a fromSHA that
// is well-formed but not an object this repository holds at all — e.g. a
// rewritten history whose old commits have since been garbage-collected.
// This must fail immediately rather than risk the walk hitting its cap
// before ever confirming fromSHA doesn't exist (see the bounded-vs-unknown
// note on CommitCountBetween's doc comment).
func TestCommitCountBetween_FromAbsentFromRepoIsAnError(t *testing.T) {
	root := t.TempDir()
	to := seedInitRepo(t, root)

	_, _, err := CommitCountBetween(root, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", to, 500)
	if err == nil {
		t.Fatal("CommitCountBetween: want error when fromSHA is not an object in the repository, got nil")
	}
}

func TestCommitCountBetween_UnresolvableToShaIsAnError(t *testing.T) {
	root := t.TempDir()
	from := seedInitRepo(t, root)

	_, _, err := CommitCountBetween(root, from, "not-a-valid-sha", 500)
	if err == nil {
		t.Fatal("CommitCountBetween: want error for an unresolvable toSHA, got nil")
	}
}

func TestCommitCountBetween_UnresolvableFromShaIsAnError(t *testing.T) {
	root := t.TempDir()
	to := seedInitRepo(t, root)

	_, _, err := CommitCountBetween(root, "not-a-valid-sha", to, 500)
	if err == nil {
		t.Fatal("CommitCountBetween: want error for an unresolvable fromSHA, got nil")
	}
}

func TestCommitCountBetween_OutsideRepositoryIsAnError(t *testing.T) {
	root := t.TempDir()

	_, _, err := CommitCountBetween(root, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", 500)
	if err == nil {
		t.Fatal("CommitCountBetween: want error outside any repository, got nil")
	}
}
