// Package git provides a Source implementation that clones a remote Git
// repository and polls it on a configurable interval. Task directories are
// discovered with the same snapshot-diff approach as the local source.
package git

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v7"
	"github.com/dicode/dicode/internal/gitops"
	"github.com/dicode/dicode/pkg/source"
	"github.com/dicode/dicode/pkg/task"
	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	gogittransport "github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"go.uber.org/zap"
)

// stripURLCredentials returns rawURL with any userinfo (user:password@) removed.
// Returns rawURL unchanged if it cannot be parsed or has no userinfo.
func stripURLCredentials(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.User == nil {
		return rawURL
	}
	u.User = nil
	return u.String()
}

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
	// Deterministic local dir name from credential-stripped URL so re-adding
	// the same repo with different credentials reuses the existing clone.
	displayURL := stripURLCredentials(url)
	h := sha256.Sum256([]byte(displayURL))
	dir := filepath.Join(dataDir, "repos", fmt.Sprintf("%x", h[:8]))

	return &GitSource{
		id:           displayURL,
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
		g.log.Warn("git source: initial clone/pull failed", zap.String("url", g.id), zap.Error(err))
	}

	ch := make(chan source.Event, 32)
	if err := g.syncAndEmit(ctx, ch); err != nil {
		g.log.Warn("git source: initial scan failed", zap.String("url", g.id), zap.Error(err))
	}

	go g.poll(ctx, ch)
	return ch, nil
}

// Sync triggers an immediate pull + rescan.
//
// May block up to cloneRetryMaxElapsed (~30s) on transient failures, since
// the underlying clone-or-pull retries with exponential backoff. Callers on
// interactive paths (UI buttons, request handlers) should pass a ctx with a
// shorter deadline if they need to fail fast.
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
				g.log.Warn("git source: pull failed", zap.String("url", g.id), zap.Error(err))
				continue
			}
			if err := g.syncAndEmit(ctx, ch); err != nil {
				g.log.Warn("git source: scan failed", zap.String("url", g.id), zap.Error(err))
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

	_, err := backoff.Retry(ctx, func() (struct{}, error) {
		err := op(ctx)
		if err == nil {
			return struct{}{}, nil
		}
		// Don't burn cycles retrying broken config: auth failures and
		// "repo not found" are operator errors, not transient ones.
		if isPermanentGitError(err) {
			return struct{}{}, backoff.Permanent(err)
		}
		return struct{}{}, err
	}, backoff.WithBackOff(bo), backoff.WithMaxElapsedTime(cloneRetryMaxElapsed))

	// Retry reports every failure as *backoff.RetryError; callers and logs
	// want the underlying git error, not the retry machinery.
	if re := backoff.AsRetryError(err); re != nil {
		return re.LastErr
	}
	return err
}

// tryCloneOrPull executes a single clone-or-refresh attempt against the remote.
// Returns nil on success; any error indicates a failure the caller
// (cloneOrPull) may want to retry.
func (g *GitSource) tryCloneOrPull(ctx context.Context) error {
	ref := plumbing.NewBranchReferenceName(g.branch)
	return gitops.CloneAtRef(ctx, g.localDir, g.url, ref, gitops.HTTPAuth(g.tokenEnv))
}

// isPermanentGitError reports whether err is a configuration-style failure
// that retrying cannot fix. The poll loop will re-attempt on the next tick
// regardless; classifying these as Permanent just avoids burning the
// 30-second retry budget every poll interval.
//
// Conservatively scoped: only the unambiguous "your credentials/URL are
// wrong" sentinels from go-git's transport layer, plus gitops.ValidateRemoteHost's
// SSRF-guard rejection (gitops.ErrBlockedHost / gitops.ErrNoRemoteHost) — a
// deterministic, zero-I/O check whose outcome cannot change within a single
// poll interval, so retrying it burns the full retry budget for no benefit
// (see #510: without this case a single SSRF-blocked source stalled the
// reconciler's initial sync by ~30s, since GitSource.Start calls cloneOrPull
// synchronously and sources start up sequentially). Everything else (network
// timeout, 5xx, packfile decode error mid-clone, partial response, …) is
// treated as transient.
func isPermanentGitError(err error) bool {
	switch {
	case errors.Is(err, gogittransport.ErrAuthenticationRequired),
		errors.Is(err, gogittransport.ErrAuthorizationFailed),
		errors.Is(err, gogittransport.ErrInvalidAuthMethod),
		errors.Is(err, gogittransport.ErrRepositoryNotFound),
		errors.Is(err, gitops.ErrBlockedHost),
		errors.Is(err, gitops.ErrNoRemoteHost):
		return true
	}
	return false
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
//
// Remotes on loopback/private/link-local/internal hosts are rejected before
// any connection is attempted — this function is reachable from the REST API
// with a caller-supplied URL, so it must not be usable to probe the daemon's
// internal network (#475). Uses gitops.ValidateRemoteHost, the same shared
// guard CloneOrPull calls (#489), so there is exactly one place that can
// drift out of sync.
func ListBranches(ctx context.Context, repoURL, tokenEnv string) ([]string, error) {
	if err := gitops.ValidateRemoteHost(repoURL); err != nil {
		return nil, err
	}

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
