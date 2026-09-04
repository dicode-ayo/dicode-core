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
// branches and tags alike. The clone is a throwaway mirror this package has
// always overwritten — nothing written into it survives a refresh — so a reset
// is what both cases want, and it drops the ways a merge can fail against a
// remote that rewound.
//
// A ref the remote re-points is therefore followed. Immutability is not this
// layer's job: the approval gate re-pends any task whose content hash changed,
// and dicode.lock records the version an operator approved.
//
// The worktree is left alone when the ref did not move, so fsnotify only fires
// on a refresh that actually changed files — pkg/taskset's watch loop depends
// on that to skip a re-resolve after a no-op poll.
//
// auth may be nil for public repos.
//
// Clones are full (no Depth limit) so go-git always has the ancestry it needs
// when the remote advances. See #175.
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

// syncToRef fetches ref into the clone at dir and hard-resets the worktree
// onto it, or returns without touching the worktree when the ref has not moved.
func syncToRef(ctx context.Context, dir string, ref plumbing.ReferenceName, auth *http.BasicAuth) error {
	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		return fmt.Errorf("open repo: %w", err)
	}
	// HEAD is read before the fetch: fetching a branch updates the very ref a
	// symbolic HEAD resolves through, so a comparison made afterwards would
	// always report "unchanged" and the worktree would never be updated.
	before := plumbing.ZeroHash
	if head, headErr := repo.Head(); headErr == nil {
		before = head.Hash()
	}
	if err := fetchRef(ctx, repo, ref, auth); err != nil {
		return err
	}
	hash, err := repo.ResolveRevision(plumbing.Revision(ref))
	if err != nil {
		return fmt.Errorf("%w: %s", ErrRefNotFound, ref)
	}
	if before == *hash {
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

// fetchRef fetches one ref by name, overwriting the local copy so a rewound
// branch or a re-cut tag is followed rather than refused as a non-fast-forward.
func fetchRef(ctx context.Context, repo *gogit.Repository, ref plumbing.ReferenceName, auth *http.BasicAuth) error {
	opts := &gogit.FetchOptions{
		RemoteName: "origin",
		RefSpecs:   []gogitconfig.RefSpec{gogitconfig.RefSpec("+" + ref + ":" + ref)},
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
