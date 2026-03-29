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
// It resolves the full task tree on startup and on each poll cycle, diffs the
// result against the previous snapshot, and emits Added/Updated/Removed events.
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

// Start performs an initial resolution, emits events, then polls for changes.
// The returned channel is closed when ctx is cancelled.
func (s *Source) Start(ctx context.Context) (<-chan source.Event, error) {
	ch := make(chan source.Event, 64)
	s.mu.Lock()
	s.ch = ch
	s.mu.Unlock()

	if err := s.syncAndEmit(ctx, ch); err != nil {
		s.log.Warn("taskset source: initial resolution failed",
			zap.String("id", s.id), zap.Error(err))
		// Non-fatal: keep polling in case the repo becomes accessible.
	}

	if !s.rootRef.IsGit() {
		go s.watchLocal(ctx, ch)
	} else {
		go s.poll(ctx, ch)
	}
	return ch, nil
}

// SetDevMode enables or disables dev mode for this source.
// localPath, when non-empty, overrides the root entry point to that local yaml path.
// Triggers an immediate re-sync so changes are reflected in the registry.
func (s *Source) SetDevMode(ctx context.Context, enabled bool, localPath string) error {
	s.resolver.SetDevMode(enabled)
	s.mu.Lock()
	s.devRootPath = localPath
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

// watchLocal uses fsnotify to react to local file changes immediately (debounced
// at 150 ms) rather than waiting for the poll interval. Falls back to poll if
// fsnotify is unavailable. A background ticker still fires at pollInterval to
// catch any directories that weren't watched at the time they were created.
func (s *Source) watchLocal(ctx context.Context, ch chan<- source.Event) {
	defer close(ch)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		s.log.Warn("taskset: fsnotify unavailable, falling back to poll",
			zap.String("id", s.id), zap.Error(err))
		s.pollLoop(ctx, ch)
		return
	}
	defer watcher.Close()

	// Always watch the directory that contains the root taskset.yaml.
	rootDir := filepath.Dir(s.rootRef.Path)
	_ = watcher.Add(rootDir)

	// Watch all task directories already in the snapshot.
	s.addSnapshotDirs(watcher)

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

	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()

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
			timerC = nil
			if err := s.syncAndEmit(ctx, ch); err != nil {
				s.log.Warn("taskset source: watch sync failed",
					zap.String("id", s.id), zap.Error(err))
			}
			// Pick up any new task directories added by the re-sync.
			s.addSnapshotDirs(watcher)
		case <-ticker.C:
			if err := s.syncAndEmit(ctx, ch); err != nil {
				s.log.Warn("taskset source: poll failed",
					zap.String("id", s.id), zap.Error(err))
			}
			s.addSnapshotDirs(watcher)
		}
	}
}

// addSnapshotDirs adds every task directory in the current snapshot to watcher
// (silently ignores duplicates — fsnotify deduplicates internally).
func (s *Source) addSnapshotDirs(watcher *fsnotify.Watcher) {
	s.mu.Lock()
	dirs := make([]string, 0, len(s.snapshot))
	for _, snap := range s.snapshot {
		dirs = append(dirs, snap.taskDir)
	}
	s.mu.Unlock()
	for _, d := range dirs {
		_ = watcher.Add(d)
	}
}

func (s *Source) poll(ctx context.Context, ch chan<- source.Event) {
	defer close(ch)
	s.pollLoop(ctx, ch)
}

func (s *Source) pollLoop(ctx context.Context, ch chan<- source.Event) {
	defer close(ch)
	ticker := time.NewTicker(s.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.resolver.InvalidateClones() // force re-pull from remote on each tick
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

	return s.resolver.Resolve(ctx, s.namespace, rootRef, configDefaults, nil)
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
