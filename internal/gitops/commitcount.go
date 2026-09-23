package gitops

import (
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
)

// CommitCountBetween returns the number of commits strictly after fromSHA
// and up to and including toSHA, walking toSHA's *first-parent* history in
// the repository that tracks dir (see HeadInfo). Counting reads commit
// objects only, never a tree or a blob.
//
// First-parent only, deliberately: dicode's git sources each track one
// branch, and a merge commit's first parent is that branch's own previous
// tip (the shape every git host's "merge pull request" produces) — so this
// counts commits that landed on the tracked branch, the same thing
// `git log --first-parent --count` would report, and never double-counts or
// walks into a merged-in feature branch's own history. A full ancestry walk
// (following every parent) would count those side-branch commits too and
// cannot terminate at a single, well-defined fromSHA the way a first-parent
// walk can: two branches sharing a fromSHA reach it at different distances
// depending which parent is followed first.
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
// err is returned when toSHA cannot be resolved to a commit at all (no
// repository, or the hash is not a valid commit there). Walking off the
// root of history, or off a first-parent chain that never passes through
// fromSHA (fromSHA reachable only via a non-first parent, a rewritten
// history, a divergent branch switch, or fromSHA belonging to a different
// lineage entirely) without ever reaching it, is reported the same way,
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
