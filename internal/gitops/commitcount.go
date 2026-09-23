package gitops

import (
	"fmt"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// CommitCountBetween returns the number of commits strictly after fromSHA
// and up to and including toSHA, in the repository that tracks dir (see
// HeadInfo), walking back from toSHA. Counting reads commit objects only,
// never a tree or a blob.
//
// fromSHA == toSHA returns (0, false, nil) without opening the repository:
// there is nothing between a commit and itself.
//
// The walk stops as soon as it has seen limit commits without reaching
// fromSHA: count is limit and bounded is true, meaning "at least this many"
// rather than an exact count — this is what caps the cost of a large
// history rather than walking all of it. bounded is false when the walk
// reached fromSHA before the cap, and count is then exact.
//
// err is returned when toSHA cannot be resolved to a commit at all (no
// repository, or the hash is not a valid commit there). Walking off the
// root of history without ever reaching fromSHA — a rewritten history, a
// divergent branch switch, or fromSHA belonging to a different lineage
// entirely — is reported the same way, since "some number of commits" would
// be a claim this walk cannot back. Callers that treat the count as
// optional decoration should discard the error and omit it, the same as
// every other "what moved" signal (see ADR-0001).
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
	if _, err := repo.CommitObject(toHash); err != nil {
		return 0, false, fmt.Errorf("resolve commit %s: %w", toSHA, err)
	}
	fromHash := plumbing.NewHash(fromSHA)

	iter, err := repo.Log(&gogit.LogOptions{From: toHash})
	if err != nil {
		return 0, false, fmt.Errorf("walk commit log from %s: %w", toSHA, err)
	}
	defer iter.Close()

	n := 0
	for {
		c, err := iter.Next()
		if err != nil {
			// Exhausted history (io.EOF) without ever reaching fromSHA, or a
			// repository fault mid-walk — neither can back a count.
			return 0, false, fmt.Errorf("walk commit log from %s toward %s: %w", toSHA, fromSHA, err)
		}
		if c.Hash == fromHash {
			return n, false, nil
		}
		n++
		if n >= limit {
			return n, true, nil
		}
	}
}
