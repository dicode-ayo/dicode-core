package taskset

import (
	"context"

	"github.com/dicode/dicode/internal/gitops"
)

// cloneOrPull clones url@branch into dir if not present, otherwise pulls.
// tokenEnv is the name of an env var holding an HTTP auth token; pass "" for public repos.
//
// Delegates to internal/gitops.CloneOrPull which handles re-clone-on-corrupt
// recovery (see #175, #176).
func cloneOrPull(ctx context.Context, dir, url, branch, tokenEnv string) error {
	return gitops.CloneOrPull(ctx, dir, url, branch, gitops.HTTPAuth(tokenEnv))
}

// isReclonableError reports whether the local clone is in a state that
// blowing-it-away-and-re-cloning fixes.
func isReclonableError(err error) bool {
	return gitops.IsReclonableError(err)
}
