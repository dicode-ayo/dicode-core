package gitops

import (
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
)

// CommitCountBetween returns the number of commits strictly after fromSHA
// and up to and including toSHA, walking toSHA's first-parent history in the
// repository that tracks dir (see HeadInfo) — the same commits
// `git log --first-parent --count` would report. Counting reads commit
// objects only, never a tree or a blob.
//
// fromSHA == toSHA returns (0, false, nil) without opening the repository:
// there is nothing between a commit and itself.
//
// The walk stops as soon as it has visited limit commits without reaching
// fromSHA: count is limit and bounded is true, meaning "at least this many"
// rather than an exact count — this is what caps the cost of a large
// history rather than walking all of it. bounded is false when the walk
// reached fromSHA before the cap, and count is then exact.
//
// A bounded result does not distinguish a first-parent chain that is
// genuinely longer than limit from one that never reaches fromSHA at all —
// both look identical within the cap's budget, since confirming the latter
// would mean walking to the root regardless of limit. Callers that render
// this as "N+" are making that same "at least" claim, not an exact one.
//
// err is returned when fromSHA or toSHA is not a valid commit hash, or
// toSHA cannot be resolved to a commit in this repository at all (no
// repository, or the hash is absent from it). fromSHA is resolved the same
// way, so a fromSHA that no longer exists in the object store at all — e.g.
// a rewritten history whose old commits were since garbage-collected —
// fails here rather than risking the ambiguity above. Walking off the root
// of a first-parent chain without ever reaching a fromSHA that does still
// exist in the repository (reachable only via a non-first parent, or
// belonging to a different lineage entirely) is reported the same way,
// since "some number of commits" would be a claim this walk cannot back.
// Callers that treat the count as optional decoration should discard the
// error and omit it, the same as every other "what moved" signal (see
// ADR-0001).
func CommitCountBetween(dir, fromSHA, toSHA string, limit int) (count int, bounded bool, err error) {
	if fromSHA == toSHA {
		return 0, false, nil
	}
	repo, err := openRepo(dir)
	if err != nil {
		return 0, false, err
	}
	toHash := plumbing.NewHash(toSHA)
	if toHash.IsZero() {
		return 0, false, fmt.Errorf("resolve commit %s: not a valid commit hash", toSHA)
	}
	current, err := repo.CommitObject(toHash)
	if err != nil {
		return 0, false, fmt.Errorf("resolve commit %s: %w", toSHA, err)
	}
	fromHash := plumbing.NewHash(fromSHA)
	if fromHash.IsZero() {
		return 0, false, fmt.Errorf("resolve commit %s: not a valid commit hash", fromSHA)
	}
	if _, err := repo.CommitObject(fromHash); err != nil {
		return 0, false, fmt.Errorf("resolve commit %s: %w", fromSHA, err)
	}

	n := 0
	for {
		if current.Hash == fromHash {
			return n, false, nil
		}
		n++
		if n >= limit {
			return n, true, nil
		}
		if current.NumParents() == 0 {
			return 0, false, fmt.Errorf("walk first-parent history from %s toward %s: reached a root commit without finding it", toSHA, fromSHA)
		}
		current, err = current.Parent(0)
		if err != nil {
			return 0, false, fmt.Errorf("walk first-parent history from %s toward %s: %w", toSHA, fromSHA, err)
		}
	}
}
