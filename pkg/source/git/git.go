// Package git provides a Source implementation that clones a remote Git
// repository and polls it on a configurable interval. Task directories are
// discovered with the same snapshot-diff approach as the local source.
package git

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/dicode/dicode/pkg/source"
	"github.com/dicode/dicode/pkg/task"
	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	gogittransport "github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"go.uber.org/zap"
)

const (
	defaultPoll = 30 * time.Second

	// cloneRetryMaxElapsed caps total time spent on bounded retries inside
	// cloneOrPull. Long enough to ride out a transient hiccup, short enough
	// that the next poll tick happens roughly on schedule even if every
	// attempt fails.
	cloneRetryMaxElapsed = 30 * time.Second
)

// GitSource clones + polls a remote repository and emits task change events.
type GitSource struct {
	id           string
	url          string
	branch       string
	pollInterval time.Duration
	localDir     string // where the repo is cloned on disk

	// auth
	tokenEnv string // env var holding HTTP basic-auth token (GitHub PAT etc.)
	sshKey   string // path to SSH private key (unused for now)

	mu       sync.Mutex
	snapshot map[string]string // taskID → hash

	// cloneOrPullOp, when non-nil, is invoked by cloneOrPull instead of
	// the production tryCloneOrPull path. Tests use this to verify retry
	// behaviour without standing up a real git server.
	cloneOrPullOp cloneOrPullFn

	log *zap.Logger
}

// New creates a GitSource.
//   - dataDir: base directory for clones (e.g. ~/.dicode/repos)
//   - url:     git remote URL
//   - branch:  branch to track (default "main")
//   - poll:    how often to pull (default 30s)
//   - tokenEnv: env var name holding an HTTP Bearer / Basic-auth token; "" = none
//   - sshKey:  path to SSH private key; "" = none
func New(dataDir, url, branch string, poll time.Duration, tokenEnv, sshKey string, log *zap.Logger) (*GitSource, error) {
	if branch == "" {
		branch = "main"
	}
	if poll == 0 {
		poll = defaultPoll
	}
	// Deterministic local dir name from URL hash so re-adding the same URL reuses the clone.
	h := sha256.Sum256([]byte(url))
	dir := filepath.Join(dataDir, "repos", fmt.Sprintf("%x", h[:8]))

	return &GitSource{
		id:           url,
		url:          url,
		branch:       branch,
		pollInterval: poll,
		localDir:     dir,
		tokenEnv:     tokenEnv,
		sshKey:       sshKey,
		snapshot:     make(map[string]string),
		log:          log,
	}, nil
}

func (g *GitSource) ID() string { return g.id }

// Start clones (or opens) the repo, does an initial scan, then polls.
func (g *GitSource) Start(ctx context.Context) (<-chan source.Event, error) {
	if err := g.cloneOrPull(ctx); err != nil {
		// Don't fatal — the repo might be accessible later. Log and continue.
		g.log.Warn("git source: initial clone/pull failed", zap.String("url", g.url), zap.Error(err))
	}

	ch := make(chan source.Event, 32)
	if err := g.syncAndEmit(ctx, ch); err != nil {
		g.log.Warn("git source: initial scan failed", zap.String("url", g.url), zap.Error(err))
	}

	go g.poll(ctx, ch)
	return ch, nil
}

// Sync triggers an immediate pull + rescan.
func (g *GitSource) Sync(ctx context.Context) error {
	if err := g.cloneOrPull(ctx); err != nil {
		return err
	}
	_, err := g.diff()
	return err
}

func (g *GitSource) poll(ctx context.Context, ch chan<- source.Event) {
	defer close(ch)
	ticker := time.NewTicker(g.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := g.cloneOrPull(ctx); err != nil {
				g.log.Warn("git source: pull failed", zap.String("url", g.url), zap.Error(err))
				continue
			}
			if err := g.syncAndEmit(ctx, ch); err != nil {
				g.log.Warn("git source: scan failed", zap.String("url", g.url), zap.Error(err))
			}
		}
	}
}

// cloneOrPullFn is the function signature cloneOrPull invokes for the actual
// network call. Extracted to a field so tests can inject a mock and verify
// retry behaviour without standing up a real git server.
type cloneOrPullFn func(ctx context.Context) error

func (g *GitSource) cloneOrPull(ctx context.Context) error {
	op := g.cloneOrPullOp
	if op == nil {
		op = g.tryCloneOrPull
	}

	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = 500 * time.Millisecond
	bo.MaxInterval = 5 * time.Second
	bo.RandomizationFactor = 0.2
	bo.Multiplier = 2
	bo.MaxElapsedTime = cloneRetryMaxElapsed
	bo.Reset()

	bctx := backoff.WithContext(bo, ctx)

	return backoff.Retry(func() error {
		err := op(ctx)
		if err == nil {
			return nil
		}
		// Don't burn cycles retrying broken config: auth failures and
		// "repo not found" are operator errors, not transient ones.
		// Wrap in backoff.Permanent so Retry surfaces them immediately.
		if isPermanentGitError(err) {
			return backoff.Permanent(err)
		}
		return err
	}, bctx)
}

// tryCloneOrPull executes a single clone-or-pull attempt against the remote.
// Returns nil on success or NoErrAlreadyUpToDate; any other error indicates
// a failure the caller (cloneOrPull) may want to retry.
func (g *GitSource) tryCloneOrPull(ctx context.Context) error {
	auth := g.httpAuth()

	// If the local dir already contains a repo, pull; otherwise clone.
	if _, err := os.Stat(filepath.Join(g.localDir, ".git")); err == nil {
		repo, err := gogit.PlainOpen(g.localDir)
		if err != nil {
			return fmt.Errorf("open repo: %w", err)
		}
		wt, err := repo.Worktree()
		if err != nil {
			return fmt.Errorf("worktree: %w", err)
		}
		opts := &gogit.PullOptions{
			RemoteName:    "origin",
			ReferenceName: plumbing.NewBranchReferenceName(g.branch),
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

	// Clone.
	if err := os.MkdirAll(g.localDir, 0755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	opts := &gogit.CloneOptions{
		URL:           g.url,
		ReferenceName: plumbing.NewBranchReferenceName(g.branch),
		SingleBranch:  true,
		Depth:         1,
	}
	if auth != nil {
		opts.Auth = auth
	}
	_, err := gogit.PlainCloneContext(ctx, g.localDir, false, opts)
	if err != nil {
		return fmt.Errorf("clone: %w", err)
	}
	return nil
}

// isPermanentGitError reports whether err is a configuration-style failure
// that retrying cannot fix. The poll loop will re-attempt on the next tick
// regardless; classifying these as Permanent just avoids burning the
// 30-second retry budget every poll interval.
//
// Conservatively scoped: only the unambiguous "your credentials/URL are
// wrong" sentinels from go-git's transport layer. Everything else (network
// timeout, 5xx, packfile decode error mid-clone, partial response, …) is
// treated as transient.
func isPermanentGitError(err error) bool {
	switch {
	case errors.Is(err, gogittransport.ErrAuthenticationRequired),
		errors.Is(err, gogittransport.ErrAuthorizationFailed),
		errors.Is(err, gogittransport.ErrInvalidAuthMethod),
		errors.Is(err, gogittransport.ErrRepositoryNotFound):
		return true
	}
	return false
}

// httpAuth returns HTTP basic-auth credentials if a token env var is set.
func (g *GitSource) httpAuth() *http.BasicAuth {
	if g.tokenEnv == "" {
		return nil
	}
	token := os.Getenv(g.tokenEnv)
	if token == "" {
		return nil
	}
	return &http.BasicAuth{Username: "git", Password: token}
}

// syncAndEmit computes a diff against the previous snapshot and sends events.
//
// Events are sent with a blocking select guarded by ctx.Done: under
// back-pressure the poller parks until the consumer drains or shutdown
// begins. A non-blocking send would silently drop events — because diff()
// has already advanced the snapshot, a dropped event is permanent
// (no next poll would re-emit it). See #178.
func (g *GitSource) syncAndEmit(ctx context.Context, ch chan<- source.Event) error {
	events, err := g.diff()
	if err != nil {
		return err
	}
	for _, ev := range events {
		select {
		case ch <- ev:
		case <-ctx.Done():
			return nil
		}
	}
	return nil
}

// ListBranches contacts the remote and returns branch names sorted alphabetically.
// tokenEnv is the name of an env var holding an HTTP auth token; pass "" for public repos.
func ListBranches(ctx context.Context, repoURL, tokenEnv string) ([]string, error) {
	ep, err := gogittransport.NewEndpoint(repoURL)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	_ = ep // endpoint validated; use go-git remote directly

	var auth gogittransport.AuthMethod
	if tokenEnv != "" {
		if token := os.Getenv(tokenEnv); token != "" {
			auth = &http.BasicAuth{Username: "git", Password: token}
		}
	}

	rem := gogit.NewRemote(nil, &gogitconfig.RemoteConfig{
		Name: "origin",
		URLs: []string{repoURL},
	})

	refs, err := rem.ListContext(ctx, &gogit.ListOptions{Auth: auth})
	if err != nil {
		return nil, fmt.Errorf("list remote: %w", err)
	}

	var branches []string
	for _, ref := range refs {
		name := ref.Name().String()
		if strings.HasPrefix(name, "refs/heads/") {
			branches = append(branches, strings.TrimPrefix(name, "refs/heads/"))
		}
	}
	sort.Strings(branches)
	return branches, nil
}

func (g *GitSource) diff() ([]source.Event, error) {
	current, err := task.ScanDir(g.localDir)
	if err != nil {
		return nil, err
	}

	g.mu.Lock()
	prev := g.snapshot
	g.snapshot = current
	g.mu.Unlock()

	// Vars injected into task.yaml template expansion for every task under
	// this source. See pkg/task/template.go and docs/task-template-vars.md.
	extras := map[string]string{task.VarTaskSetDir: g.localDir}

	added, updated, removed := source.DiffSnapshots(prev, current, func(h string) string { return h })

	events := make([]source.Event, 0, len(added)+len(updated)+len(removed))
	for _, id := range added {
		events = append(events, source.Event{
			Kind: source.EventAdded, TaskID: id, TaskDir: filepath.Join(g.localDir, id), Source: g.id, ExtraVars: extras,
		})
	}
	for _, id := range updated {
		events = append(events, source.Event{
			Kind: source.EventUpdated, TaskID: id, TaskDir: filepath.Join(g.localDir, id), Source: g.id, ExtraVars: extras,
		})
	}
	for _, id := range removed {
		events = append(events, source.Event{
			Kind: source.EventRemoved, TaskID: id, TaskDir: filepath.Join(g.localDir, id), Source: g.id,
		})
	}
	return events, nil
}
