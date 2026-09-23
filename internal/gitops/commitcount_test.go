package gitops

import (
	"fmt"
	"path/filepath"
	"testing"
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

func TestCommitCountBetween_FromNotAnAncestorIsAnError(t *testing.T) {
	root := t.TempDir()
	to := seedInitRepo(t, root)

	// A fromSHA that never appears in toSHA's ancestry (here, a commit that
	// doesn't exist at all) must not report a number — the walk exhausts
	// history without ever finding it.
	_, _, err := CommitCountBetween(root, "0000000000000000000000000000000000000000", to, 500)
	if err == nil {
		t.Fatal("CommitCountBetween: want error when fromSHA is unreachable from toSHA, got nil")
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

func TestCommitCountBetween_OutsideRepositoryIsAnError(t *testing.T) {
	root := t.TempDir()

	_, _, err := CommitCountBetween(root, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", 500)
	if err == nil {
		t.Fatal("CommitCountBetween: want error outside any repository, got nil")
	}
}
