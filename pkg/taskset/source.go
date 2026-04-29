package taskset

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// ErrDevModeBusy is returned by SetDevMode when clone-mode is already active on
// this source and a second enable call is attempted.
var ErrDevModeBusy = errors.New("dev-mode clone-mode already active on this source")

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

	mu          sync.Mutex
	snapshot    map[string]taskSnap // namespaced taskID → snapshot
	ch          chan source.Event   // live channel set by Start; nil before Start
	devRootPath string              // non-empty overrides rootRef.Path in dev mode
	watchRoot   string              // directory watched by fsnotify; set in Start
	cloneRunID  string              // non-empty while a dev-mode clone is active

	// pullStatus tracks the outcome of the most recent git pull; exposed
	// via PullStatus() for the webui source-health dot. Zero-value means
	// "never attempted" (local sources, or before Start).
	pullStatus pullStatusState
}

type taskSnap struct {
	specHash string
	spec     *task.Spec
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
) *Source {
	if pollInterval == 0 {
		pollInterval = 30 * time.Second
	}
	return &Source{
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
}

// ID implements source.Source.
func (s *Source) ID() string { return s.id }

// Start performs an initial resolution, emits events, then watches for changes.
// For git refs the root repo is cloned eagerly so fsnotify can be set up on the
// local clone directory immediately. The returned channel is closed when ctx is
// cancelled.
func (s *Source) Start(ctx context.Context) (<-chan source.Event, error) {
	ch := make(chan source.Event, 64)
	s.mu.Lock()
	s.ch = ch
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
// exclusive. Branch (and Base/RunID) are honoured by the clone-mode work in
// Tasks 3-5; this task only introduces the struct.
type DevModeOpts struct {
	LocalPath string // existing: point at a user's local taskset.yaml checkout
	Branch    string // future (Task 3): create a per-fix clone on this branch
	Base      string // future (Task 3): branch to fork from when Branch is unknown remotely
	RunID     string // future (Task 3): clone-dir name component
}

// SetDevMode enables or disables dev mode for this source.
//
// Modes:
//   - enabled=true, opts.LocalPath != "" : point dev-ref resolution at the
//     given local path (existing human-dev workflow).
//   - enabled=true, opts.Branch    != "" : clone-mode (introduced in Task 3
//     of the dev-mode-branch-lifecycle plan; not yet implemented).
//   - enabled=false : revert.
func (s *Source) SetDevMode(ctx context.Context, enabled bool, opts DevModeOpts) error {
	if opts.LocalPath != "" && opts.Branch != "" {
		return fmt.Errorf("DevModeOpts: LocalPath and Branch are mutually exclusive")
	}
	if enabled && opts.Branch != "" {
		s.mu.Lock()
		busy := s.cloneRunID != ""
		s.mu.Unlock()
		if busy {
			return ErrDevModeBusy
		}
		if err := s.enableClone(ctx, opts); err != nil {
			return err
		}
		s.resolver.SetDevMode(true)
		s.mu.Lock()
		ch := s.ch
		s.mu.Unlock()
		if ch != nil {
			return s.syncAndEmit(ctx, ch)
		}
		return nil
	}
	if !enabled {
		// If we were in clone-mode, remove the clone directory and clear runID.
		s.mu.Lock()
		runID := s.cloneRunID
		s.cloneRunID = ""
		s.mu.Unlock()
		if runID != "" {
			clonePath := filepath.Join(s.dataDir, "dev-clones", s.namespace, runID)
			if err := os.RemoveAll(clonePath); err != nil {
				// Log but don't fail — orphan sweep (dev-clones-cleanup
				// buildin, Task 13) retries. Disable must always succeed.
				s.log.Warn("dev-clones disable: removeall failed",
					zap.String("source", s.namespace),
					zap.String("path", clonePath),
					zap.Error(err),
				)
			}
		}
	}

	// existing LocalPath / disable path:
	s.resolver.SetDevMode(enabled)
	s.mu.Lock()
	s.devRootPath = opts.LocalPath
	if enabled && opts.LocalPath != "" {
		s.watchRoot = filepath.Dir(opts.LocalPath)
	}
	ch := s.ch
	s.mu.Unlock()
	if ch == nil {
		return nil // not started yet; will take effect on next Start
	}
	return s.syncAndEmit(ctx, ch)
}

// enableClone clones this source's git repo into ${dataDir}/dev-clones/<namespace>/<runID>/
// and switches devRootPath to point at the cloned taskset.yaml. If opts.Branch
// doesn't exist remotely, it is created locally from opts.Base (or the source's
// tracked branch). Pure go-git — no `git` binary.
func (s *Source) enableClone(ctx context.Context, opts DevModeOpts) error {
	if opts.RunID == "" {
		return fmt.Errorf("DevModeOpts.RunID required when Branch is set")
	}
	if err := ValidateBranchName(opts.Branch, ""); err != nil {
		return fmt.Errorf("validate branch: %w", err)
	}
	if s.rootRef == nil || s.rootRef.URL == "" {
		return fmt.Errorf("clone-mode requires a git source (rootRef.URL is empty)")
	}

	clonePath := filepath.Join(s.dataDir, "dev-clones", s.namespace, opts.RunID)
	if err := os.MkdirAll(filepath.Dir(clonePath), 0o755); err != nil {
		return fmt.Errorf("mkdir parent: %w", err)
	}

	cloneOpts := &gogit.CloneOptions{
		URL: s.rootRef.URL,
	}
	repo, err := gogit.PlainCloneContext(ctx, clonePath, false, cloneOpts)
	if err != nil {
		return fmt.Errorf("clone: %w", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("worktree: %w", err)
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
			return fmt.Errorf("checkout %q failed and no base branch resolvable: %w", opts.Branch, err)
		}
		// Try local branch ref first, then fall back to remote tracking ref.
		baseHash, resolveErr := repo.ResolveRevision(plumbing.Revision(plumbing.NewBranchReferenceName(base)))
		if resolveErr != nil {
			remoteRef := plumbing.NewRemoteReferenceName("origin", base)
			baseHash, resolveErr = repo.ResolveRevision(plumbing.Revision(remoteRef))
			if resolveErr != nil {
				return fmt.Errorf("resolve base %q: %w", base, resolveErr)
			}
		}
		if err := repo.Storer.SetReference(plumbing.NewHashReference(branchRef, *baseHash)); err != nil {
			return fmt.Errorf("create branch %q: %w", opts.Branch, err)
		}
		if err := wt.Checkout(co); err != nil {
			return fmt.Errorf("checkout %q after create: %w", opts.Branch, err)
		}
	}

	// devRootPath points at the cloned root taskset.yaml.
	rootEntry := s.rootRef.Path
	if rootEntry == "" {
		rootEntry = "taskset.yaml"
	}
	s.mu.Lock()
	s.devRootPath = filepath.Join(clonePath, rootEntry)
	s.cloneRunID = opts.RunID
	s.mu.Unlock()
	return nil
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
			specHash: hashSpec(rt.Spec),
			spec:     rt.Spec,
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
			Kind: source.EventAdded, TaskID: id, TaskDir: cur.taskDir, Source: s.id, Spec: cur.spec,
		})
	}
	for _, id := range updated {
		cur := current[id]
		s.send(ch, source.Event{
			Kind: source.EventUpdated, TaskID: id, TaskDir: cur.taskDir, Source: s.id, Spec: cur.spec,
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
	return s.resolver.Resolve(ctx, s.namespace, rootRef, configDefaults, nil, nil)
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

func hashSpec(spec *task.Spec) string {
	b, _ := json.Marshal(spec)
	h := sha256.Sum256(b)
	return fmt.Sprintf("%x", h)
}
