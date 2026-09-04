package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dicode/dicode/internal/fsutil"
	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

// ErrRefNotFound reports a ref the remote does not publish.
//
// IsReclonableError must not match it: a mistyped branch or tag is a
// configuration error that re-cloning cannot fix, and go-git's own "reference
// not found" wording would otherwise send the caller through
// wipe-and-re-clone against the remote on every poll tick.
var ErrRefNotFound = errors.New("ref not found on remote")

// CloneAtRef clones url into dir at ref, or brings an existing clone onto it.
// ref is fully qualified — refs/heads/<branch> or refs/tags/<tag>.
//
// Refreshing is a fetch of that one ref followed by a hard reset onto it,
// branches and tags alike. Nothing written into the clone survives a refresh.
//
// A ref the remote re-points is followed. Freezing content is the approval
// gate's job, not this layer's.
//
// The worktree is left alone when the ref did not move: pkg/taskset's watch
// loop re-resolves on fsnotify, so a refresh that rewrote unchanged files
// would drive a re-resolve on every poll tick for every source.
//
// auth may be nil for public repos.
//
// Clones are full (no Depth limit) so go-git has the ancestry it needs when
// the remote advances. See #175.
func CloneAtRef(ctx context.Context, dir, url string, ref plumbing.ReferenceName, auth *http.BasicAuth) error {
	if err := ValidateRemoteHost(url); err != nil {
		return err
	}

	if fsutil.Exists(filepath.Join(dir, ".git")) {
		err := syncToRef(ctx, dir, ref, auth)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrRefNotFound) || !IsReclonableError(err) {
			return err
		}
		// Reclonable failure: wipe and fall through to the clone path.
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			return fmt.Errorf("recover clone (remove): %w", rmErr)
		}
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	opts := &gogit.CloneOptions{
		URL:           url,
		ReferenceName: ref,
		SingleBranch:  true,
	}
	if auth != nil {
		opts.Auth = auth
	}
	if _, err := gogit.PlainCloneContext(ctx, dir, false, opts); err != nil {
		if isMissingRef(err) {
			return fmt.Errorf("%w: %s", ErrRefNotFound, ref)
		}
		return fmt.Errorf("clone at %s: %w", ref, err)
	}
	return nil
}

// syncRef is where a refresh parks the commit it fetched. It is a ref the
// worktree does not track, so HEAD only ever moves as part of the reset — an
// interrupted refresh leaves HEAD behind the fetched commit, and the next
// refresh still sees the two differ and completes the checkout.
const syncRef = plumbing.ReferenceName("refs/dicode/sync-target")

// syncToRef fetches ref into the clone at dir and hard-resets the worktree
// onto it, or returns without touching the worktree when the ref has not moved.
func syncToRef(ctx context.Context, dir string, ref plumbing.ReferenceName, auth *http.BasicAuth) error {
	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		return fmt.Errorf("open repo: %w", err)
	}
	if err := fetchRef(ctx, repo, ref, auth); err != nil {
		return err
	}
	hash, err := repo.ResolveRevision(plumbing.Revision(syncRef))
	if err != nil {
		// The fetch just wrote this ref, so a miss here is the object
		// database failing to read it back rather than a ref the remote does
		// not publish — it must stay reclonable instead of being reported as
		// a mistyped ref.
		return fmt.Errorf("resolve fetched %s: %w", ref, err)
	}
	head, err := repo.Head()
	if err == nil && head.Hash() == *hash {
		return nil
	}
	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("worktree: %w", err)
	}
	// HardReset moves whatever HEAD is: the branch it points at when symbolic,
	// HEAD itself when detached at a tag.
	if err := wt.Reset(&gogit.ResetOptions{Mode: gogit.HardReset, Commit: *hash}); err != nil {
		return fmt.Errorf("reset to %s: %w", ref, err)
	}
	return nil
}

// fetchRef fetches one ref by name into syncRef, overwriting it so a rewound
// branch or a re-cut tag is followed rather than refused as a non-fast-forward.
func fetchRef(ctx context.Context, repo *gogit.Repository, ref plumbing.ReferenceName, auth *http.BasicAuth) error {
	opts := &gogit.FetchOptions{
		RemoteName: "origin",
		RefSpecs:   []gogitconfig.RefSpec{gogitconfig.RefSpec("+" + ref + ":" + syncRef)},
		Force:      true,
	}
	if auth != nil {
		opts.Auth = auth
	}
	if err := repo.FetchContext(ctx, opts); err != nil && err != gogit.NoErrAlreadyUpToDate {
		if isMissingRef(err) {
			return fmt.Errorf("%w: %s", ErrRefNotFound, ref)
		}
		return fmt.Errorf("fetch %s: %w", ref, err)
	}
	return nil
}

// isMissingRef reports whether err is go-git's way of saying the remote does
// not publish the ref that was asked for.
func isMissingRef(err error) bool {
	return errors.Is(err, gogit.NoMatchingRefSpecError{}) || errors.Is(err, plumbing.ErrReferenceNotFound)
}
