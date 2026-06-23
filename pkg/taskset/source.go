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
		// Clone-mode enable: reserve a slot for this RunID.
		s.mu.Lock()
		if s.clones == nil {
			s.clones = make(map[string]cloneState)
		}
		if _, exists := s.clones[opts.RunID]; exists {
			s.mu.Unlock()
			return ErrDevModeBusy
		}
		s.clones[opts.RunID] = cloneState{} // placeholder; populated after clone
		s.mu.Unlock()

		devPath, err := s.enableClone(ctx, opts)
		if err != nil {
			s.mu.Lock()
			delete(s.clones, opts.RunID)
			s.mu.Unlock()
			return err
		}

		s.mu.Lock()
		if _, stillReserved := s.clones[opts.RunID]; !stillReserved {
			// A concurrent disable-all removed our placeholder while enableClone
			// ran outside the lock. The clone directory was successfully created,
			// so clean it up to leave no orphan on disk.
			s.mu.Unlock()
			_ = os.RemoveAll(filepath.Dir(devPath))
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
			if !strings.HasPrefix(item.cloneDir+string(filepath.Separator), cloneRoot+string(filepath.Separator)) {
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
// remotely, it is created locally from opts.Base (or the source's tracked branch).
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

	// Build the clone path defensively. ValidateRunID above already rejects
	// any opts.RunID containing '/', '..', or other traversal characters
	// (regex: ^[A-Za-z0-9_-]{1,64}$), but we re-verify the joined result is
	// rooted at the expected parent directory so static analysers (CodeQL)
	// can see the safety property without tracing through ValidateRunID.
	cloneRoot := filepath.Join(s.dataDir, "dev-clones", s.namespace)
	clonePath := filepath.Join(cloneRoot, opts.RunID)
	cleanClonePath := filepath.Clean(clonePath)
	if cleanClonePath != clonePath || !strings.HasPrefix(cleanClonePath+string(filepath.Separator), cloneRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("clone path escapes data dir: %q", clonePath)
	}
	if err := os.MkdirAll(cloneRoot, 0o755); err != nil {
		return "", fmt.Errorf("mkdir parent: %w", err)
	}

	// Remove any partial clone on failure so a retry with the same RunID isn't
	// wedged by go-git "repository already exists". cleanClonePath is safe to
	// use here — it has already been verified to sit inside cloneRoot above.
	var cloneSucceeded bool
	defer func() {
		if !cloneSucceeded {
			_ = os.RemoveAll(cleanClonePath)
		}
	}()

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
		base := opts.Base
		if base == "" {
			base = s.rootRef.Branch
		}
		if base == "" {
			return "", fmt.Errorf("checkout %q failed and no base branch resolvable: %w", opts.Branch, err)
		}
		// Try local branch ref first, then fall back to remote tracking ref.
		baseHash, resolveErr := repo.ResolveRevision(plumbing.Revision(plumbing.NewBranchReferenceName(base)))
		if resolveErr != nil {
			remoteRef := plumbing.NewRemoteReferenceName("origin", base)
			baseHash, resolveErr = repo.ResolveRevision(plumbing.Revision(remoteRef))
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
	cloneSucceeded = true
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

// Namespace returns this source's root namespace segment.
func (s *Source) Namespace() string { return s.namespace }

// Sync triggers an immediate re-resolution without emitting events.
func (s *Source) Sync(ctx context.Context) error {
	_, err := s.resolve(ctx)
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
	tasks, err := s.resolve(ctx)
	if err != nil {
		return err
	}

	current := make(map[string]taskSnap, len(tasks))
	for _, rt := range tasks {
		current[rt.ID] = taskSnap{
			specHash: hashKinded(rt.Kinded),
			kinded:   rt.Kinded,
			taskDir:  rt.TaskDir,
		}
	}

	s.mu.Lock()
	prev := s.snapshot
	s.snapshot = current
	s.mu.Unlock()

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

func (s *Source) resolve(ctx context.Context) ([]*ResolvedTask, error) {
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
