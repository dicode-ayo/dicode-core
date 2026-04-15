// Package local provides a Source implementation that watches a local directory
// for task changes using fsnotify. Suitable for development and local-only mode.
package local

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dicode/dicode/pkg/source"
	"github.com/dicode/dicode/pkg/task"
	"github.com/fsnotify/fsnotify"
	"go.uber.org/zap"
)

const debounce = 150 * time.Millisecond

// LocalSource watches a local directory for task changes.
type LocalSource struct {
	id   string
	path string // absolute path to the tasks directory

	mu       sync.Mutex
	snapshot map[string]string // taskID → hash at last sync

	log *zap.Logger
}

// New creates a LocalSource for the given directory.
func New(id, path string, log *zap.Logger) (*LocalSource, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return &LocalSource{
		id:       id,
		path:     abs,
		snapshot: make(map[string]string),
		log:      log,
	}, nil
}

func (s *LocalSource) ID() string { return s.id }

// Start performs an initial sync, then watches for filesystem changes.
// Events are debounced (150ms) to handle editors that write via tmp-rename.
func (s *LocalSource) Start(ctx context.Context) (<-chan source.Event, error) {
	ch := make(chan source.Event, 32)

	// Initial scan — emit Added for every task already on disk.
	if err := s.syncAndEmit(ch); err != nil {
		close(ch)
		return nil, err
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		close(ch)
		return nil, err
	}
	// Watch root tasks dir and all current task subdirectories.
	if err := s.addWatchDirs(watcher); err != nil {
		watcher.Close()
		close(ch)
		return nil, err
	}

	go s.watch(ctx, watcher, ch)
	return ch, nil
}

// addWatchDirs adds the root tasks dir and each task subdirectory to the watcher.
func (s *LocalSource) addWatchDirs(watcher *fsnotify.Watcher) error {
	if err := watcher.Add(s.path); err != nil {
		return err
	}
	entries, err := os.ReadDir(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			_ = watcher.Add(filepath.Join(s.path, e.Name()))
		}
	}
	return nil
}

// Sync triggers an immediate rescan and emits any changes.
func (s *LocalSource) Sync(ctx context.Context) error {
	// We emit to a local sink because callers of Sync don't consume events.
	// Callers that need events go through Start.
	_ = ctx
	_, err := s.diff()
	return err
}

func (s *LocalSource) watch(ctx context.Context, watcher *fsnotify.Watcher, ch chan<- source.Event) {
	defer watcher.Close()
	defer close(ch)

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

	fire := func() {
		timerC = nil
		if err := s.syncAndEmit(ch); err != nil {
			s.log.Warn("local source sync error", zap.String("path", s.path), zap.Error(err))
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			s.log.Warn("watcher error", zap.Error(err))
		case _, ok := <-watcher.Events:
			if !ok {
				return
			}
			resetTimer()
		case <-timerC:
			fire()
		}
	}
}

// syncAndEmit computes a diff against the previous snapshot and sends events.
func (s *LocalSource) syncAndEmit(ch chan<- source.Event) error {
	events, err := s.diff()
	if err != nil {
		return err
	}
	for _, ev := range events {
		select {
		case ch <- ev:
		default:
			s.log.Warn("local source event channel full, dropping event",
				zap.String("task", ev.TaskID))
		}
	}
	return nil
}

// diff computes what changed since the last snapshot and returns events.
// It updates the snapshot atomically.
func (s *LocalSource) diff() ([]source.Event, error) {
	current, err := task.ScanDir(s.path)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	prev := s.snapshot
	s.snapshot = current
	s.mu.Unlock()

	var events []source.Event

	// Vars injected into task.yaml template expansion for every task under
	// this source. See pkg/task/template.go and docs/task-template-vars.md.
	extras := map[string]string{task.VarTaskSetDir: s.path}

	for id, hash := range current {
		dir := filepath.Join(s.path, id)
		if _, ok := prev[id]; !ok {
			events = append(events, source.Event{
				Kind: source.EventAdded, TaskID: id, TaskDir: dir, Source: s.id, ExtraVars: extras,
			})
		} else if prev[id] != hash {
			events = append(events, source.Event{
				Kind: source.EventUpdated, TaskID: id, TaskDir: dir, Source: s.id, ExtraVars: extras,
			})
		}
	}

	for id := range prev {
		if _, ok := current[id]; !ok {
			events = append(events, source.Event{
				Kind: source.EventRemoved, TaskID: id, TaskDir: filepath.Join(s.path, id), Source: s.id,
			})
		}
	}

	return events, nil
}
