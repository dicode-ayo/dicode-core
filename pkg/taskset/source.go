package taskset

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bep/debounce"
	"github.com/dicode/dicode/internal/gitops"
	"github.com/dicode/dicode/internal/pathguard"
	"github.com/dicode/dicode/pkg/source"
	"github.com/dicode/dicode/pkg/task"
	"github.com/fsnotify/fsnotify"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"go.uber.org/zap"
)

// ErrDevModeBusy is returned by SetDevMode when a clone session with the
// same RunID is already active on this source. Use distinct RunIDs to run
// multiple concurrent sessions.
var ErrDevModeBusy = errors.New("dev-mode: a clone session with this run ID is already active")

// Source implements source.Source using a TaskSet yaml file as its entry point.
// It resolves the full task tree on startup and on each change cycle, diffs the
// result against the previous snapshot, and emits Added/Updated/Removed events.
//
// For local sources fsnotify is used to react to file changes immediately
// (debounced at 150 ms). For git sources a periodic ticker pulls from the
// remote; fsnotify on the local clone directory then detects actual file
// changes so syncAndEmit only runs when content has changed.
type Source struct {
	id         string
	namespace  string
	rootRef    *Ref
	configPath string // optional path to a kind:Config file

	resolver     *Resolver
	pollInterval time.Duration
	log          *zap.Logger

	// dataDir is the daemon's base data directory (e.g. ~/.dicode).
	// It mirrors the resolver's private dataDir and is kept here so that
	// clone-mode (enableClone) can compute its own subdirectory paths
	// without reaching into the resolver's internals.
	dataDir string

	mu           sync.Mutex
	snapshot     map[string]taskSnap   // namespaced taskID → snapshot
	ch           chan source.Event     // live channel set by Start; nil before Start
	refresh      chan struct{}         // signals an out-of-band re-resolve; set by Start
	devRootPath  string                // non-empty overrides rootRef.Path in dev mode
	watchRoot    string                // directory watched by fsnotify; set in Start
	clones       map[string]cloneState // active clone sessions keyed by runID
	primaryRunID string                // runID whose clone the resolver surfaces

	// pullStatus tracks the outcome of the most recent git pull; exposed
	// via PullStatus() for the webui source-health dot. Zero-value means
	// "never attempted" (local sources, or before Start).
	pullStatus pullStatusState

	// loadFailures tracks entries that failed to resolve/load/validate on the
	// most recent sync — exposed via LoadFailures() so the webui can keep
	// them visible instead of vanishing (#649). Replaced wholesale on every
	// syncAndEmit so an entry that starts resolving cleanly again (or is
	// genuinely removed from the taskset spec) drops out automatically.
	failuresMu sync.RWMutex
	failures   map[string]task.LoadFailure

	parentOverrides *Overrides // overrides applied at the dicode.yaml entry level
}

// cloneState holds the per-session state for a dev-mode clone.
type cloneState struct {
	cloneDir    string // absolute path of the cloned repo directory (pre-validated)
	devRootPath string // absolute path to the root taskset.yaml inside the clone
	branch      string
	base        string
	createdAt   time.Time
}

// SourceOption configures a Source at construction time.
type SourceOption func(*Source)

// WithParentOverrides binds the dicode.yaml entry-level overrides to the
// source so the resolver applies them on every resolve. Without this the
// daemon-built source would silently drop spec.entries.<src>.overrides.
func WithParentOverrides(ov *Overrides) SourceOption {
	return func(s *Source) { s.parentOverrides = ov }
}

// WithAllowedTokenEnvs installs the operator's source_security.
// allowed_token_envs allowlist (#753) on the source's resolver. Without this
// every git ref's auth.token_env stays unrestricted (the resolver's default),
// which is the correct behavior when the operator never configured an
// allowlist — see Resolver.SetAllowedTokenEnvs.
func WithAllowedTokenEnvs(envs []string) SourceOption {
	return func(s *Source) { s.resolver.SetAllowedTokenEnvs(envs) }
}

type taskSnap struct {
	specHash string
	kinded   task.Kinded
	taskDir  string
}

// NewSource creates a TaskSet Source.
//   - id:           unique source identifier (e.g. the root repo URL or local path)
//   - namespace:    root namespace segment (e.g. "infra")
//   - rootRef:      ref pointing to the root taskset.yaml
//   - configPath:   optional path to a kind:Config yaml (pass "" to auto-discover)
//   - dataDir:      base directory for cloned repos (e.g. ~/.dicode)
//   - devMode:      if true, dev_ref substitutions are applied
//   - pollInterval: how often to re-resolve and diff (0 → 30s)
func NewSource(
	id, namespace string,
	rootRef *Ref,
	configPath string,
	dataDir string,
	devMode bool,
	pollInterval time.Duration,
	log *zap.Logger,
	opts ...SourceOption,
) *Source {
	if pollInterval == 0 {
		pollInterval = 30 * time.Second
	}
	s := &Source{
		id:           id,
		namespace:    namespace,
		rootRef:      rootRef,
		configPath:   configPath,
		dataDir:      dataDir,
		resolver:     NewResolver(dataDir, devMode, log),
		pollInterval: pollInterval,
		log:          log,
		snapshot:     make(map[string]taskSnap),
		failures:     make(map[string]task.LoadFailure),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// ID implements source.Source.
func (s *Source) ID() string { return s.id }

// RepoPath returns the on-disk path of this source's git repo. For sources in
// clone-mode (dev-mode clone active) it returns the primary clone directory;
// otherwise it returns the cached pull dir established in Start.
func (s *Source) RepoPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.primaryRunID != "" {
		return filepath.Join(s.dataDir, "dev-clones", s.namespace, s.primaryRunID)
	}
	return s.watchRoot
}

// RootTaskSetPath returns the absolute path of the taskset file this source
// currently resolves from — the dev-mode override when one is active, the
// clone-local root for a git ref, otherwise the local ref itself. A ref
// pointing at a directory yields the taskset.yaml inside it, matching the file
// resolution picks up. Returns "" before Start has established the watch root
// of a git source.
//
// Callers that write into a source (task scaffolding, task removal) need this
// rather than RepoPath: the entry they add or drop lives in this file, and its
// directory — not the repo root — is what a relative ref resolves against.
func (s *Source) RootTaskSetPath() string {
	s.mu.Lock()
	devRootPath := s.devRootPath
	watchRoot := s.watchRoot
	s.mu.Unlock()

	if devRootPath != "" && s.resolver.DevMode() {
		return devRootPath
	}

	path := s.rootRef.Path
	if s.rootRef.IsGit() {
		if watchRoot == "" {
			return ""
		}
		if path == "" {
			path = "taskset.yaml"
		}
		path = filepath.Join(watchRoot, path)
	}
	path = resolveYAMLPath(filepath.Clean(path))
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		path = filepath.Join(path, "taskset.yaml")
	}
	return path
}

// DurableRoot reports whether a file written beside RootTaskSetPath survives
// the next sync. A local ref does, and so does a git ref resolved through a
// dev-mode clone — a working tree. A git ref resolved through the pull cache
// does not: gitops pulls with Force: true and discards anything written there.
//
// The dev-mode condition below is the one RootTaskSetPath itself applies, and
// deliberately so: reading DevMode() alone would answer true during a dev-mode
// disable, which clears devRootPath before it lowers the resolver's flag, in
// which case RootTaskSetPath has already fallen back to the pull cache.
func (s *Source) DurableRoot() bool {
	if !s.rootRef.IsGit() {
		return true
	}
	s.mu.Lock()
	devRootPath := s.devRootPath
	s.mu.Unlock()
	return devRootPath != "" && s.resolver.DevMode()
}

// Start performs an initial resolution, emits events, then watches for changes.
// For git refs the root repo is cloned eagerly so fsnotify can be set up on the
// local clone directory immediately. The returned channel is closed when ctx is
// cancelled.
func (s *Source) Start(ctx context.Context) (<-chan source.Event, error) {
	ch := make(chan source.Event, 64)
	s.mu.Lock()
	s.ch = ch
	s.refresh = make(chan struct{}, 1)
	s.mu.Unlock()

	// Determine (and cache) the local directory to watch.
	watchRoot, err := s.resolver.Pull(ctx, s.rootRef)
	s.recordPull(err)
	if err != nil {
		s.log.Warn("taskset source: initial clone/pull failed",
			zap.String("id", s.id), zap.Error(err))
		// Non-fatal: still try to sync; pull will be retried on the next tick.
	}
	s.mu.Lock()
	s.watchRoot = watchRoot
	s.mu.Unlock()

	if err := s.syncAndEmit(ctx, ch); err != nil {
		s.log.Warn("taskset source: initial resolution failed",
			zap.String("id", s.id), zap.Error(err))
	}

	go s.watch(ctx, ch)
	return ch, nil
}

// DevModeOpts configures dev-mode activation. LocalPath and Branch are mutually
// exclusive.
type DevModeOpts struct {
	LocalPath string // point at a user's local taskset.yaml checkout
	Branch    string // create a per-fix clone checked out to this branch
	Base      string // branch to fork from when Branch is unknown remotely
	RunID     string // clone-dir name component (validated by ValidateRunID)
}

// SetDevMode enables or disables dev mode for this source.
//
// Modes:
//   - enabled=true, opts.LocalPath != "" : point dev-ref resolution at the
//     given local path (existing human-dev workflow).
//   - enabled=true, opts.Branch    != "" : clone-mode — clones the source
//     repo into a per-run subdirectory of the data-dir and checks out the
//     requested branch.
//   - enabled=false : revert to the primary source ref.
func (s *Source) SetDevMode(ctx context.Context, enabled bool, opts DevModeOpts) error {
	if opts.LocalPath != "" && opts.Branch != "" {
		return fmt.Errorf("DevModeOpts: LocalPath and Branch are mutually exclusive")
	}

	if enabled && opts.Branch != "" {
		// Validate RunID early so callers receive ErrInvalidRunID for bad IDs,
		// preserving the existing API contract (enableClone also validates, but
		// the path-safety check below would otherwise fire first).
		if err := ValidateRunID(opts.RunID); err != nil {
			return fmt.Errorf("validate run id: %w", err)
		}

		// Pre-compute the expected clone directory and store it in the placeholder
		// cloneState before calling enableClone. On failure the error-path reads
		// the directory back from the map VALUE (not from opts.RunID directly),
		// which breaks CodeQL's taint flow from opts.RunID into os.RemoveAll.
		cloneRoot := filepath.Join(s.dataDir, "dev-clones", s.namespace)
		expectedCloneDir := filepath.Join(cloneRoot, opts.RunID)
		cleanExpected := filepath.Clean(expectedCloneDir)
		sep := string(filepath.Separator)
		if cleanExpected != expectedCloneDir || !strings.HasPrefix(cleanExpected+sep, cloneRoot+sep) {
			return fmt.Errorf("dev-mode: clone path escapes data dir: %q", expectedCloneDir)
		}

		s.mu.Lock()
		if s.clones == nil {
			s.clones = make(map[string]cloneState)
		}
		if _, exists := s.clones[opts.RunID]; exists {
			s.mu.Unlock()
			return ErrDevModeBusy
		}
		// Pre-fill cloneDir so that the error-path cleanup below can reach it
		// via a map-value read (which does not propagate CodeQL taint from the key).
		s.clones[opts.RunID] = cloneState{cloneDir: cleanExpected}
		s.mu.Unlock()

		devPath, err := s.enableClone(ctx, opts)
		if err != nil {
			s.mu.Lock()
			// Read the pre-filled cloneDir from the map VALUE before deleting the
			// entry. This breaks the CodeQL taint path from opts.RunID into
			// os.RemoveAll while still cleaning up any partial clone on disk.
			var dirToClean string
			if cs, ok := s.clones[opts.RunID]; ok {
				dirToClean = cs.cloneDir
			}
			delete(s.clones, opts.RunID)
			s.mu.Unlock()
			if dirToClean != "" {
				_ = os.RemoveAll(dirToClean)
			}
			return err
		}

		s.mu.Lock()
		if _, stillReserved := s.clones[opts.RunID]; !stillReserved {
			// A concurrent disable-all removed our placeholder while enableClone
			// ran outside the lock. The clone directory was successfully created;
			// the orphan will be cleaned on the next retry (enableClone will
			// fail, and the error-path above will RemoveAll via the map value).
			s.mu.Unlock()
			return fmt.Errorf("dev-mode: session %q cancelled by concurrent disable-all", opts.RunID)
		}
		s.clones[opts.RunID] = cloneState{
			cloneDir:    filepath.Dir(devPath),
			devRootPath: devPath,
			branch:      opts.Branch,
			base:        opts.Base,
			createdAt:   time.Now(),
		}
		s.primaryRunID = opts.RunID
		s.devRootPath = devPath
		ch := s.ch
		s.mu.Unlock()

		s.resolver.SetDevMode(true)
		if ch != nil {
			return s.syncAndEmit(ctx, ch)
		}
		return nil
	}

	if !enabled {
		// sessionToRemove pairs a runID with the pre-validated clone directory
		// path stored in cloneState. Using the stored path (a map value set
		// during enableClone after ValidateRunID) rather than reconstructing from
		// opts.RunID at disable time breaks the user-controlled-data taint flow
		// into os.RemoveAll.
		type sessionToRemove struct {
			runID    string
			cloneDir string
		}

		s.mu.Lock()
		// Collect sessions to remove. Empty RunID means "disable all".
		var toRemove []sessionToRemove
		if opts.RunID != "" {
			if cs, ok := s.clones[opts.RunID]; ok {
				toRemove = []sessionToRemove{{runID: opts.RunID, cloneDir: cs.cloneDir}}
			}
		} else {
			for runID, cs := range s.clones {
				toRemove = append(toRemove, sessionToRemove{runID: runID, cloneDir: cs.cloneDir})
			}
		}
		for _, item := range toRemove {
			delete(s.clones, item.runID)
		}
		// Recompute primary and devRootPath.
		wasPrimary := opts.RunID == "" || opts.RunID == s.primaryRunID
		if wasPrimary {
			if len(s.clones) > 0 {
				s.primaryRunID = s.latestCloneRunIDLocked()
				s.devRootPath = s.clones[s.primaryRunID].devRootPath
			} else {
				s.primaryRunID = ""
				s.devRootPath = ""
			}
		}
		hasClonesLeft := len(s.clones) > 0
		s.mu.Unlock()

		// Remove clone directories outside the lock using the stored, pre-validated
		// paths from cloneState (set by enableClone). Defence-in-depth: verify each
		// stored path is still rooted within the expected parent directory.
		cloneRoot := filepath.Join(s.dataDir, "dev-clones", s.namespace)
		for _, item := range toRemove {
			if item.cloneDir == "" {
				continue
			}
			if within, err := pathguard.Within(cloneRoot, item.cloneDir); err != nil || !within {
				s.log.Warn("dev-clones disable: stored clone path escapes data dir; refusing to remove",
					zap.String("source", s.namespace),
					zap.String("path", item.cloneDir),
				)
				continue
			}
			if err := os.RemoveAll(item.cloneDir); err != nil {
				s.log.Warn("dev-clones disable: removeall failed",
					zap.String("source", s.namespace),
					zap.String("path", item.cloneDir),
					zap.Error(err),
				)
			}
		}

		if !hasClonesLeft {
			s.resolver.SetDevMode(false)
			s.mu.Lock()
			s.devRootPath = opts.LocalPath // "" for plain disables
			s.mu.Unlock()
		}

		s.mu.Lock()
		ch := s.ch
		s.mu.Unlock()
		if ch == nil {
			return nil
		}
		return s.syncAndEmit(ctx, ch)
	}

	// LocalPath enable path (enabled=true, opts.LocalPath != "").
	s.resolver.SetDevMode(enabled)
	s.mu.Lock()
	s.devRootPath = opts.LocalPath
	if enabled && opts.LocalPath != "" {
		s.watchRoot = filepath.Dir(opts.LocalPath)
	}
	ch := s.ch
	s.mu.Unlock()
	if ch == nil {
		return nil
	}
	return s.syncAndEmit(ctx, ch)
}

// latestCloneRunIDLocked returns the runID with the most recent createdAt.
// Caller must hold s.mu.
func (s *Source) latestCloneRunIDLocked() string {
	var bestID string
	var bestTime time.Time
	for id, cs := range s.clones {
		if cs.createdAt.After(bestTime) {
			bestTime = cs.createdAt
			bestID = id
		}
	}
	return bestID
}

// enableClone clones this source's git repo into ${dataDir}/dev-clones/<namespace>/<runID>/
// and returns the path to the cloned taskset.yaml. If opts.Branch doesn't exist
// remotely, it is created locally from opts.Base (or the source's tracked
// branch, or the tag it is pinned to).
// Pure go-git — no `git` binary.
func (s *Source) enableClone(ctx context.Context, opts DevModeOpts) (string, error) {
	if opts.RunID == "" {
		return "", fmt.Errorf("DevModeOpts.RunID required when Branch is set")
	}
	if err := ValidateRunID(opts.RunID); err != nil {
		return "", fmt.Errorf("validate run id: %w", err)
	}
	// TODO(#238): pass per-task branch_prefix once auto-fix override wires it.
	// branch_prefix enforcement is deferred to #238 (auto-fix taskset override
	// where the prefix config is wired). Local format validity is sufficient here.
	if err := ValidateBranchName(opts.Branch, ""); err != nil {
		return "", fmt.Errorf("validate branch: %w", err)
	}
	if s.rootRef == nil || s.rootRef.URL == "" {
		return "", fmt.Errorf("clone-mode requires a git source (rootRef.URL is empty)")
	}
	// SSRF guard (#489/#510): enableClone drives a real go-git clone against
	// a caller-influenced URL (reachable via SetDevMode from pkg/webui/sources.go
	// and pkg/webui/task_delete.go), so it must go through the same shared
	// literal-host check as CloneOrPull and ListBranches before any network
	// operation happens — otherwise it's a third, unmitigated SSRF entry point.
	if err := gitops.ValidateRemoteHost(s.rootRef.URL); err != nil {
		return "", fmt.Errorf("validate remote host: %w", err)
	}

	// Build the clone path defensively. ValidateRunID above already rejects
	// any opts.RunID containing '/', '..', or other traversal characters
	// (regex: ^[A-Za-z0-9_-]{1,64}$), but we re-verify the joined result is
	// rooted at the expected parent directory so static analysers (CodeQL)
	// can see the safety property without tracing through ValidateRunID.
	cloneRoot := filepath.Join(s.dataDir, "dev-clones", s.namespace)
	clonePath := filepath.Join(cloneRoot, opts.RunID)
	cleanClonePath := filepath.Clean(clonePath)
	within, werr := pathguard.Within(cloneRoot, cleanClonePath)
	if cleanClonePath != clonePath || werr != nil || !within {
		return "", fmt.Errorf("clone path escapes data dir: %q", clonePath)
	}
	if err := os.MkdirAll(cloneRoot, 0o755); err != nil {
		return "", fmt.Errorf("mkdir parent: %w", err)
	}

	cloneOpts := &gogit.CloneOptions{
		URL: s.rootRef.URL,
	}
	repo, err := gogit.PlainCloneContext(ctx, clonePath, false, cloneOpts)
	if err != nil {
		return "", fmt.Errorf("clone: %w", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("worktree: %w", err)
	}

	branchRef := plumbing.NewBranchReferenceName(opts.Branch)
	co := &gogit.CheckoutOptions{Branch: branchRef}
	if err := wt.Checkout(co); err != nil {
		// branch doesn't exist — create it locally from Base
		// The fix has to be cut from what the source actually runs, which on a
		// pinned source is the tag rather than any branch.
		base := opts.Base
		if base == "" {
			base = s.rootRef.TrackedName()
		}
		if base == "" {
			return "", fmt.Errorf("checkout %q failed and no base branch resolvable: %w", opts.Branch, err)
		}
		// Try local branch ref first, then the remote tracking ref, then a tag.
		baseHash, resolveErr := repo.ResolveRevision(plumbing.Revision(plumbing.NewBranchReferenceName(base)))
		if resolveErr != nil {
			remoteRef := plumbing.NewRemoteReferenceName("origin", base)
			baseHash, resolveErr = repo.ResolveRevision(plumbing.Revision(remoteRef))
		}
		if resolveErr != nil {
			baseHash, resolveErr = repo.ResolveRevision(plumbing.Revision(plumbing.NewTagReferenceName(base)))
			if resolveErr != nil {
				return "", fmt.Errorf("resolve base %q: %w", base, resolveErr)
			}
		}
		if err := repo.Storer.SetReference(plumbing.NewHashReference(branchRef, *baseHash)); err != nil {
			return "", fmt.Errorf("create branch %q: %w", opts.Branch, err)
		}
		if err := wt.Checkout(co); err != nil {
			return "", fmt.Errorf("checkout %q after create: %w", opts.Branch, err)
		}
	}

	rootEntry := s.rootRef.Path
	if rootEntry == "" {
		rootEntry = "taskset.yaml"
	}
	return filepath.Join(clonePath, rootEntry), nil
}

// DevMode reports whether dev mode is currently active.
func (s *Source) DevMode() bool { return s.resolver.DevMode() }

// DevRootPath returns the current dev-mode local path override (empty if none).
func (s *Source) DevRootPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.devRootPath
}

// DataDir returns the daemon data directory used for source clones.
func (s *Source) DataDir() string { return s.dataDir }

// LoadFailures returns a snapshot of every entry that failed to resolve,
// load, or validate on this source's most recent sync, keyed by entry ID
// (#649). Empty (never nil) once at least one sync has completed cleanly.
func (s *Source) LoadFailures() map[string]task.LoadFailure {
	s.failuresMu.RLock()
	defer s.failuresMu.RUnlock()
	out := make(map[string]task.LoadFailure, len(s.failures))
	for k, v := range s.failures {
		out[k] = v
	}
	return out
}

// setLoadFailures replaces the recorded failure set wholesale with the
// outcome of the sync that just ran. Replacing rather than merging means an
// entry that now resolves cleanly, or was removed from the taskset spec
// entirely, drops out on its own — no separate "clear" call needed.
func (s *Source) setLoadFailures(fails []ResolveFailure) {
	m := make(map[string]task.LoadFailure, len(fails))
	now := time.Now()
	for _, f := range fails {
		m[f.ID] = task.LoadFailure{ID: f.ID, Source: s.id, Error: f.Error.Error(), At: now}
	}
	s.failuresMu.Lock()
	s.failures = m
	s.failuresMu.Unlock()
}

// Namespace returns this source's root namespace segment.
func (s *Source) Namespace() string { return s.namespace }

// Sync triggers an immediate re-resolution without emitting events.
func (s *Source) Sync(ctx context.Context) error {
	_, _, err := s.resolve(ctx)
	return err
}

// SetParentOverrides updates the source's entry-level overrides and signals
// an out-of-band re-resolve. Safe to call on a running source. Used by
// PATCH /api/tasks/{id}/overrides to apply toggle changes without waiting
// for the next poll tick.
func (s *Source) SetParentOverrides(ov *Overrides) {
	s.mu.Lock()
	s.parentOverrides = ov
	refresh := s.refresh
	s.mu.Unlock()
	if refresh == nil {
		return // not started yet; will take effect on Start's initial resolve
	}
	select {
	case refresh <- struct{}{}:
	default: // signal already pending; coalesce
	}
}

// watch is the unified file-watching loop for both local and git sources.
//
//   - For local sources:  fsnotify reacts directly to edits; a background
//     ticker re-registers any new task directories added since last sync.
//   - For git sources:    a pull ticker fetches from the remote on every
//     pollInterval; fsnotify then fires only when the pull actually changed
//     files on disk, so syncAndEmit is skipped on no-op pulls.
//
// Falls back to a plain polling loop if fsnotify is unavailable.
func (s *Source) watch(ctx context.Context, ch chan<- source.Event) {
	defer close(ch)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		s.log.Warn("taskset: fsnotify unavailable, falling back to poll",
			zap.String("id", s.id), zap.Error(err))
		s.pollFallback(ctx, ch)
		return
	}
	defer watcher.Close()

	s.addWatchDirs(watcher)

	// bep/debounce schedules its callback in a detached goroutine with no
	// Stop() in v1.2.1. To keep watcher and channel mutation panic-free on
	// shutdown we use the debouncer only to coalesce events, and hand the
	// actual fire back into this goroutine via a cap-1 signal channel.
	// fireSig is never closed; a late post-shutdown trigger becomes a
	// harmless no-op when the buffer is already full.
	const debounceInterval = 150 * time.Millisecond
	debounced := debounce.New(debounceInterval)
	fireSig := make(chan struct{}, 1)
	trigger := func() {
		select {
		case fireSig <- struct{}{}:
		default:
		}
	}

	// Pull ticker — only for git sources; nil for local.
	var pullTickC <-chan time.Time
	if s.rootRef.IsGit() {
		pt := time.NewTicker(s.pollInterval)
		defer pt.Stop()
		pullTickC = pt.C
	}

	// Re-registration ticker picks up newly created task directories that
	// weren't watched at the time they were first created.
	reregTicker := time.NewTicker(s.pollInterval)
	defer reregTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			s.log.Warn("taskset watcher error", zap.String("id", s.id), zap.Error(err))
		case _, ok := <-watcher.Events:
			if !ok {
				return
			}
			debounced(trigger)
		case <-fireSig:
			if err := s.syncAndEmit(ctx, ch); err != nil {
				s.log.Warn("taskset source: sync failed",
					zap.String("id", s.id), zap.Error(err))
			}
			s.addWatchDirs(watcher)
		case <-s.refresh:
			if err := s.syncAndEmit(ctx, ch); err != nil {
				s.log.Warn("taskset: refresh-driven syncAndEmit failed",
					zap.String("id", s.id), zap.Error(err))
			}
		case <-pullTickC:
			// Fetch from remote. If the pull actually changed files on disk,
			// fsnotify will fire and trigger syncAndEmit via the debounce path.
			_, err := s.resolver.Pull(ctx, s.rootRef)
			s.recordPull(err)
			if err != nil {
				s.log.Warn("taskset source: pull failed",
					zap.String("id", s.id), zap.Error(err))
			}
		case <-reregTicker.C:
			// Re-register any task directories that appeared since last sync.
			s.addWatchDirs(watcher)
		}
	}
}

// addWatchDirs registers the watch-root and all current snapshot task
// directories with the watcher. Duplicates are silently ignored by fsnotify.
func (s *Source) addWatchDirs(watcher *fsnotify.Watcher) {
	s.mu.Lock()
	root := s.watchRoot
	dirs := make([]string, 0, len(s.snapshot))
	for _, snap := range s.snapshot {
		dirs = append(dirs, snap.taskDir)
	}
	s.mu.Unlock()

	if root != "" {
		_ = watcher.Add(root)
	}
	for _, d := range dirs {
		_ = watcher.Add(d)
	}
}

// pollFallback is a plain ticker loop used when fsnotify is unavailable.
// For git sources it pulls before each sync; for local sources it just syncs.
func (s *Source) pollFallback(ctx context.Context, ch chan<- source.Event) {
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.refresh:
			if err := s.syncAndEmit(ctx, ch); err != nil {
				s.log.Warn("taskset: refresh-driven syncAndEmit failed",
					zap.String("id", s.id), zap.Error(err))
			}
		case <-ticker.C:
			if s.rootRef.IsGit() {
				_, err := s.resolver.Pull(ctx, s.rootRef)
				s.recordPull(err)
				if err != nil {
					s.log.Warn("taskset source: pull failed",
						zap.String("id", s.id), zap.Error(err))
				}
			}
			if err := s.syncAndEmit(ctx, ch); err != nil {
				s.log.Warn("taskset source: poll failed",
					zap.String("id", s.id), zap.Error(err))
			}
		}
	}
}

func (s *Source) syncAndEmit(ctx context.Context, ch chan<- source.Event) error {
	tasks, failures, err := s.resolve(ctx)
	if err != nil {
		return err
	}

	current := make(map[string]taskSnap, len(tasks))
	for _, rt := range tasks {
		current[rt.ID] = taskSnap{
			specHash: s.snapHash(rt.Kinded, rt.TaskDir, rt.ID),
			kinded:   rt.Kinded,
			taskDir:  rt.TaskDir,
		}
	}

	s.mu.Lock()
	prev := s.snapshot
	// An entry that failed to resolve this pass must not look "removed" to
	// DiffSnapshots below (#649) — that's the exact mechanism that made a
	// broken task.yaml vanish instead of surfacing an error: the resolver
	// already omits failed entries from `tasks`/`current`, so without this,
	// any entry with a prior snapshot would emit EventRemoved and the
	// reconciler would unregister a previously-good task. Carrying the prior
	// snapshot entry forward unchanged keeps its hash stable — no event
	// fires for it at all — while setLoadFailures below still records the
	// failure for the webui. A brand-new entry failing for the first time has
	// no prior snapshot to carry forward, so it simply doesn't appear in
	// `current` (never added) until it resolves cleanly; it's still visible
	// via the load-failure side channel even though it never registers.
	//
	// A failure's ID is not always a leaf task's own ID: when a nested
	// `kind: TaskSet` entry itself fails to resolve (e.g. its taskset.yaml is
	// unparseable), resolveNestedRef reports exactly one ResolveFailure keyed
	// on the nested group's namespace (e.g. "infra/subgroup"), even though
	// every previously-resolved leaf task under that namespace (e.g.
	// "infra/subgroup/deploy") also vanished from `current` this pass. Those
	// leaf IDs were never registered under "infra/subgroup" itself — only
	// their own namespaced IDs exist in `prev` — so an exact-key lookup alone
	// misses them and DiffSnapshots would see them as removed. Carry forward
	// every prev entry whose ID is f.ID itself OR a namespace-descendant of it
	// (ID == f.ID, or ID has prefix f.ID+"/") to cover both the leaf-failure
	// case (ID == f.ID, no descendants) and the nested-group-failure case
	// (f.ID's descendants) uniformly.
	for _, f := range failures {
		for id, snap := range prev {
			if id != f.ID && !strings.HasPrefix(id, f.ID+"/") {
				continue
			}
			if _, already := current[id]; !already {
				current[id] = snap
			}
		}
	}
	s.snapshot = current
	s.mu.Unlock()

	s.setLoadFailures(failures)

	added, updated, removed := source.DiffSnapshots(prev, current, func(t taskSnap) string { return t.specHash })

	for _, id := range added {
		cur := current[id]
		s.send(ch, source.Event{
			Kind: source.EventAdded, TaskID: id, TaskDir: cur.taskDir, Source: s.id, Kinded: cur.kinded,
		})
	}
	for _, id := range updated {
		cur := current[id]
		s.send(ch, source.Event{
			Kind: source.EventUpdated, TaskID: id, TaskDir: cur.taskDir, Source: s.id, Kinded: cur.kinded,
		})
	}
	for _, id := range removed {
		s.send(ch, source.Event{
			Kind: source.EventRemoved, TaskID: id, Source: s.id,
		})
	}
	return nil
}

func (s *Source) send(ch chan<- source.Event, ev source.Event) {
	select {
	case ch <- ev:
	default:
		s.log.Warn("taskset source: event channel full, dropping",
			zap.String("task", ev.TaskID))
	}
}

func (s *Source) resolve(ctx context.Context) ([]*ResolvedTask, []ResolveFailure, error) {
	configDefaults, err := s.loadConfigDefaults()
	if err != nil {
		s.log.Warn("taskset source: config load failed",
			zap.String("path", s.configPath), zap.Error(err))
		// Non-fatal — proceed without config defaults.
	}

	rootRef := s.rootRef
	s.mu.Lock()
	devRootPath := s.devRootPath
	s.mu.Unlock()
	if devRootPath != "" && s.resolver.DevMode() {
		rootRef = &Ref{Path: devRootPath}
	}

	// TASK_SET_DIR is injected by Resolver.Resolve itself from the resolved
	// root taskset.yaml path, so the source loader no longer needs to know
	// about it. Pass nil for extraVars — if a future source type needs to
	// layer additional vars, build them here.
	// parentOverrides is read under mu because SetParentOverrides may be
	// called concurrently (e.g. from PATCH /api/tasks/{id}/overrides).
	s.mu.Lock()
	parent := s.parentOverrides
	s.mu.Unlock()
	return s.resolver.Resolve(ctx, s.namespace, rootRef, configDefaults, parent, nil)
}

func (s *Source) loadConfigDefaults() (*Defaults, error) {
	cfgPath := s.configPath
	if cfgPath == "" {
		// Auto-discover alongside the root ref.
		if !s.rootRef.IsGit() {
			cfgPath = filepath.Join(filepath.Dir(s.rootRef.Path), "dicode-config.yaml")
		}
		// For git refs the config path is resolved after clone; skip auto-discover here.
	}
	if cfgPath == "" {
		return nil, nil
	}
	cs, err := LoadConfig(cfgPath)
	if err != nil {
		return nil, err
	}
	if cs == nil {
		return nil, nil
	}
	return cs.Spec.Defaults, nil
}

// hashKinded computes a content hash for change detection over any resolved
// task kind. *task.Spec and *task.PipelineTask both marshal to distinct JSON,
// so a kind change (or any field change) yields a different hash.
func hashKinded(k task.Kinded) string {
	b, _ := json.Marshal(k)
	h := sha256.Sum256(b)
	return fmt.Sprintf("%x", h)
}

// snapHash is the change-detection hash for one resolved task. It folds both
// the resolved spec and the task directory's file content: the runtime imports
// script files (task.js/task.ts and siblings) that the resolved spec does not
// capture, so a script-only edit must still perturb the hash — otherwise no
// update is emitted and the approval gate never re-pends the changed task
// (#530). A dir-less inline task hashes its spec alone.
//
// It's a method (not a free function) so the error path below can log
// through s.log — see the #682 comment on that path for why silence there
// is unacceptable.
func (s *Source) snapHash(k task.Kinded, taskDir, taskID string) string {
	specHash := hashKinded(k)
	if taskDir == "" {
		return specHash
	}
	var hashInclude []string
	if sp, ok := k.(*task.Spec); ok {
		hashInclude = sp.HashInclude
	}
	dirHash, err := task.Hash(taskDir, hashInclude...)
	if err == nil {
		return specHash + ":" + dirHash
	}

	// #682: task.Hash can fail here for several reasons (an unreadable
	// taskDir, a broken in-dir symlink's readlink call) but the one this fix
	// targets is a hash_include entry escaping its sibling-task boundary
	// through a symlink — a case the load-time lexical pre-check in
	// pkg/task/spec.go can't catch, because the escape is only visible after
	// the symlink is resolved (see
	// TestHash_IncludeThroughSymlinkedIntermediateDirIsRejected). Whatever
	// the cause, this used to fall back to specHash alone, silently dropping
	// the ENTIRE directory
	// — including the task's own script — from the change-detection
	// identity: a script edit then changed no spec field, the hash stayed
	// stable, syncAndEmit saw no diff, and the approval gate was never
	// re-armed for the edit. That's a silent approval-gate bypass, not
	// merely a missed reload, so the error must be loud and must still
	// perturb the identity — not just get folded away.
	//
	// Best-effort fallback: hash taskDir alone, without hash_include. This
	// narrower hash skips the includes loop entirely, so it's unaffected by
	// a failure there regardless of which entry is escaping or where it
	// points — not because hash_include is guaranteed to stay outside
	// taskDir (a zero-".."-hop entry like "subdir/file.txt" passes the
	// lexical check in pkg/task/spec.go and does resolve inside taskDir).
	// It keeps catching ordinary script edits for as long as the include
	// stays broken — it doesn't just fire once and go blind again. err.Error()
	// is static for a given (path, boundary) pair, so folding its digest in
	// is deterministic across repeated polls with nothing else changed: one
	// loud transition into "changed" per new edit, not a firehose on every
	// ~30s reconciler tick. A full redesign that propagates this error to
	// callers instead of folding it into the hash is tracked separately
	// (#688) — out of scope here.
	//
	// This re-walks taskDir a second time (task.Hash above already walked it
	// once before failing on the includes step) for as long as the failure
	// persists. Accepted trade-off: the failure is a misconfiguration an
	// operator is expected to fix, not steady-state behavior, and reusing
	// the first walk's entries would mean threading walkTree's internal
	// result across Hash's public boundary for a cold-path cost.
	s.log.Warn("taskset source: task dir hash failed, treating task as changed",
		zap.String("task", taskID),
		zap.String("taskDir", taskDir),
		zap.Error(err),
	)
	fallback, fbErr := task.Hash(taskDir)
	if fbErr != nil {
		// taskDir itself is unreadable (rare — hash_include escaping is the
		// documented failure mode, not this). Nothing left to fold in beyond
		// the error digest below.
		fallback = ""
	}
	errDigest := sha256.Sum256([]byte(err.Error()))
	return specHash + ":" + fallback + ":hash-error:" + fmt.Sprintf("%x", errDigest)
}
