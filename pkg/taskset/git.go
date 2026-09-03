package taskset

import (
	"context"

	"github.com/dicode/dicode/internal/gitops"
)

// syncClone brings the clone at dir onto tgt, cloning it first if it is not
// there yet. tokenEnv is the name of an env var holding an HTTP auth token;
// pass "" for public repos.
//
// The two targets refresh differently: a branch is pulled, because the remote
// head moves; a tag names one commit for good, so it is checked out and only
// fetched when the clone has never seen it.
//
// Delegates to internal/gitops, which handles re-clone-on-corrupt recovery
// (see #175, #176).
func syncClone(ctx context.Context, dir, url string, tgt gitTarget, tokenEnv string) error {
	auth := gitops.HTTPAuth(tokenEnv)
	if tgt.isPinned() {
		return gitops.CloneAtTag(ctx, dir, url, tgt.Tag, auth)
	}
	return gitops.CloneOrPull(ctx, dir, url, tgt.Branch, auth)
}

// isReclonableError reports whether the local clone is in a state that
// blowing-it-away-and-re-cloning fixes.
func isReclonableError(err error) bool {
	return gitops.IsReclonableError(err)
}
