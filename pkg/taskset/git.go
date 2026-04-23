package taskset

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

// cloneOrPull clones url@branch into dir if not present, otherwise pulls.
// tokenEnv is the name of an env var holding an HTTP auth token; pass "" for public repos.
func cloneOrPull(ctx context.Context, dir, url, branch, tokenEnv string) error {
	auth := httpAuth(tokenEnv)

	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		repo, err := gogit.PlainOpen(dir)
		if err != nil {
			return fmt.Errorf("open repo: %w", err)
		}
		wt, err := repo.Worktree()
		if err != nil {
			return fmt.Errorf("worktree: %w", err)
		}
		opts := &gogit.PullOptions{
			RemoteName:    "origin",
			ReferenceName: plumbing.NewBranchReferenceName(branch),
			Force:         true,
		}
		if auth != nil {
			opts.Auth = auth
		}
		err = wt.PullContext(ctx, opts)
		if err != nil && err != gogit.NoErrAlreadyUpToDate {
			return fmt.Errorf("pull: %w", err)
		}
		return nil
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	// Full clone (no Depth). A shallow clone costs bandwidth once but
	// fails PullContext with "object not found" the first time the remote
	// advances past the shallow tip — go-git can't compute a merge base
	// when the parent commits aren't in the local object DB. See #175.
	opts := &gogit.CloneOptions{
		URL:           url,
		ReferenceName: plumbing.NewBranchReferenceName(branch),
		SingleBranch:  true,
	}
	if auth != nil {
		opts.Auth = auth
	}
	if _, err := gogit.PlainCloneContext(ctx, dir, false, opts); err != nil {
		return fmt.Errorf("clone: %w", err)
	}
	return nil
}

func httpAuth(tokenEnv string) *http.BasicAuth {
	if tokenEnv == "" {
		return nil
	}
	token := os.Getenv(tokenEnv)
	if token == "" {
		return nil
	}
	return &http.BasicAuth{Username: "git", Password: token}
}
