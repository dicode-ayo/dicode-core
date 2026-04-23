package taskset

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/dicode/dicode/pkg/source"
	"github.com/dicode/dicode/pkg/task"
	"github.com/fsnotify/fsnotify"
	"go.uber.org/zap"
)

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

	mu          sync.Mutex
	snapshot    map[string]taskSnap // namespaced taskID → snapshot
	ch          chan source.Event   // live channel set by Start; nil before Start
	devRootPath string              // non-empty overrides rootRef.Path in dev mode
	watchRoot   string              // directory watched by fsnotify; set in Start

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

// SetDevMode enables or disables dev mode for this source.
// localPath, when non-empty, overrides the root entry point to that local yaml path.
// Triggers an immediate re-sync so changes are reflected in the registry.
func (s *Source) SetDevMode(ctx context.Context, enabled bool, localPath string) error {
	s.resolver.SetDevMode(enabled)
	s.mu.Lock()
	s.devRootPath = localPath
	if enabled && localPath != "" {
		s.watchRoot = filepath.Dir(localPath)
	}
	ch := s.ch
	s.mu.Unlock()
	if ch == nil {
		return nil // not started yet; will take effect on next Start
	}
	return s.syncAndEmit(ctx, ch)
}

// DevMode reports whether dev mode is currently active.
func (s *Source) DevMode() bool { return s.resolver.DevMode() }

// DevRootPath returns the current dev-mode local path override (empty if none).
func (s *Source) DevRootPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.devRootPath
}

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

	const debounce = 150 * time.Millisecond
	var (
		timer  *time.Timer
		timerC <-chan time.Time
	)
	resetTimer := func() {
		if timer != nil {
			timer.Stop()
		}
		timer = time.NewTimer(debounce)
		timerC = timer.C
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
			resetTimer()
		case <-timerC:
			// Debounce fired: files changed (from a local edit or a git pull).
			timerC = nil
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

	for id, cur := range current {
		var ev source.Event
		ev.TaskID = id
		ev.TaskDir = cur.taskDir
		ev.Source = s.id
		ev.Spec = cur.spec

		if _, exists := prev[id]; !exists {
			ev.Kind = source.EventAdded
		} else if prev[id].specHash != cur.specHash {
			ev.Kind = source.EventUpdated
		} else {
			continue
		}
		s.send(ch, ev)
	}

	for id := range prev {
		if _, exists := current[id]; !exists {
			s.send(ch, source.Event{
				Kind:   source.EventRemoved,
				TaskID: id,
				Source: s.id,
			})
		}
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
