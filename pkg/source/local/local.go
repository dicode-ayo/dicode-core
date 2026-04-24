// Package local provides a Source implementation that watches a local directory
// for task changes using fsnotify. Suitable for development and local-only mode.
package local

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bep/debounce"
	"github.com/dicode/dicode/pkg/source"
	"github.com/dicode/dicode/pkg/task"
	"github.com/fsnotify/fsnotify"
	"go.uber.org/zap"
)

const debounceInterval = 150 * time.Millisecond

// LocalSource watches a local directory for task changes.
type LocalSource struct {
	id           string
	path         string // absolute path to the tasks directory
	watchEnabled bool   // whether to run fsnotify; false = poll-only via explicit Sync()

	mu       sync.Mutex
	snapshot map[string]string // taskID → hash at last sync

	log *zap.Logger
}

// New creates a LocalSource for the given directory. If watchEnabled is
// false, Start performs the initial scan but does not set up fsnotify —
// callers must drive updates explicitly via Sync().
func New(id, path string, watchEnabled bool, log *zap.Logger) (*LocalSource, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return &LocalSource{
		id:           id,
		path:         abs,
		watchEnabled: watchEnabled,
		snapshot:     make(map[string]string),
		log:          log,
	}, nil
}

func (s *LocalSource) ID() string { return s.id }

// Start performs an initial sync, then watches for filesystem changes.
// Events are debounced (150ms) to handle editors that write via tmp-rename.
func (s *LocalSource) Start(ctx context.Context) (<-chan source.Event, error) {
	ch := make(chan source.Event, 32)

	// Initial scan — emit Added for every task already on disk.
	if err := s.syncAndEmit(ctx, ch); err != nil {
		close(ch)
		return nil, err
	}

	// If watch is disabled, don't spin up fsnotify. Callers drive updates
	// via Sync(). Channel stays open for the initial scan's events and is
	// closed when ctx cancels.
	if !s.watchEnabled {
		go func() {
			<-ctx.Done()
			close(ch)
		}()
		return ch, nil
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

	// bep/debounce schedules its callback via time.AfterFunc in a detached
	// goroutine and has no Stop() in v1.2.1 — so a callback scheduled just
	// before shutdown can fire after this goroutine exits and the event
	// channel is closed. To keep the send path panic-free we use the
	// debouncer only to coalesce events, and hand the actual fire back into
	// the select loop via a cap-1 signal channel. fireSig is never closed,
	// so a late post-shutdown send becomes a harmless no-op (buffer already
	// full → default branch).
	debounced := debounce.New(debounceInterval)
	fireSig := make(chan struct{}, 1)
	trigger := func() {
		select {
		case fireSig <- struct{}{}:
		default:
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
			debounced(trigger)
		case <-fireSig:
			if err := s.syncAndEmit(ctx, ch); err != nil {
				s.log.Warn("local source sync error", zap.String("path", s.path), zap.Error(err))
			}
		}
	}
}

// syncAndEmit computes a diff against the previous snapshot and sends events.
//
// Events are sent with a blocking select guarded by ctx.Done: under
// back-pressure the watcher parks until the consumer drains or shutdown
// begins. A non-blocking send would silently drop events — because diff()
// has already advanced the snapshot, a dropped event is permanent
// (no next poll would re-emit it). See #178.
func (s *LocalSource) syncAndEmit(ctx context.Context, ch chan<- source.Event) error {
	events, err := s.diff()
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

	// Vars injected into task.yaml template expansion for every task under
	// this source. See pkg/task/template.go and docs/task-template-vars.md.
	extras := map[string]string{task.VarTaskSetDir: s.path}

	added, updated, removed := source.DiffSnapshots(prev, current, func(h string) string { return h })

	events := make([]source.Event, 0, len(added)+len(updated)+len(removed))
	for _, id := range added {
		events = append(events, source.Event{
			Kind: source.EventAdded, TaskID: id, TaskDir: filepath.Join(s.path, id), Source: s.id, ExtraVars: extras,
		})
	}
	for _, id := range updated {
		events = append(events, source.Event{
			Kind: source.EventUpdated, TaskID: id, TaskDir: filepath.Join(s.path, id), Source: s.id, ExtraVars: extras,
		})
	}
	for _, id := range removed {
		events = append(events, source.Event{
			Kind: source.EventRemoved, TaskID: id, TaskDir: filepath.Join(s.path, id), Source: s.id,
		})
	}

	return events, nil
}
