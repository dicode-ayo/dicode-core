package webui

import (
	"errors"
	"fmt"
	"sync"

	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/source"
	"github.com/dicode/dicode/pkg/taskset"
	"go.uber.org/zap"
)

var (
	// ErrSourceExists reports that a spec.entries name is already claimed.
	ErrSourceExists = errors.New("source already exists")
	// ErrSourceNotFound reports that no spec.entries entry has the name.
	ErrSourceNotFound = errors.New("source not found")
	// ErrSourceStart wraps the reconciler's error when a claimed source
	// fails to start; the claim has been released by then.
	ErrSourceStart = errors.New("start source")
	// ErrSourceBusy reports that an Add for the name is still in flight.
	ErrSourceBusy = errors.New("source is being added")
	// ErrSourceRemovedConcurrently reports that a Remove took the name while
	// Add was starting it; the started source has been stopped again.
	ErrSourceRemovedConcurrently = errors.New("source removed while being added")
)

// sourceConfigStore owns cfg.Spec.Entries. update runs mutate under the
// config write lock and persists; a mutate error skips the persist and is
// returned as-is. view runs read under the config read lock.
type sourceConfigStore interface {
	update(mutate func(*config.Config) error) error
	view(read func(*config.Config))
}

// sourceReconciler starts and stops source watch loops, keyed by Source.ID.
// RemoveSource for an id whose AddSource has not yet returned is a no-op.
type sourceReconciler interface {
	AddSource(src source.Source) error
	RemoveSource(id string)
}

// sourceRegistry is the name → live source map the REST API reads.
// Unregister returns the removed source, or nil if name was not registered.
type sourceRegistry interface {
	Register(name string, src *taskset.Source)
	Unregister(name string) *taskset.Source
}

// SourceLifecycle adds and removes named sources across the config entry,
// the reconciler, and the source registry.
//
// Invariant: a source is registered, and left running in the reconciler, only
// while the config entry Add claimed for it is still the one at
// cfg.Spec.Entries[name]. The claim is identified by entry pointer, not name,
// so a late step from an older request never acts on a newer claim for the
// same name.
//
// The claim is made before the slow AddSource so starting is not serialised
// behind the config lock. A Remove landing while AddSource is in flight
// misses in the reconciler, so Add re-checks its claim afterwards and tears
// its own source down if the claim is gone. mu makes that re-check plus
// Register atomic with Remove's delete → RemoveSource → Unregister, and keeps
// a new claim from slipping between Remove's delete and its RemoveSource.
//
// Only one Add per name runs at a time: a re-add after Remove derives the
// same Source.ID, so a stale Add's teardown would otherwise stop the newer
// source. adding marks names with an Add in flight, guarded by mu.
type SourceLifecycle struct {
	mu      sync.Mutex
	adding  map[string]bool
	cfg     sourceConfigStore
	rec     sourceReconciler
	reg     sourceRegistry
	dataDir string
	log     *zap.Logger
}

func newSourceLifecycle(cfg sourceConfigStore, rec sourceReconciler, reg sourceRegistry, dataDir string, log *zap.Logger) *SourceLifecycle {
	return &SourceLifecycle{adding: map[string]bool{}, cfg: cfg, rec: rec, reg: reg, dataDir: dataDir, log: log}
}

// Add claims name for ref, starts the source, and registers it. ref must
// already be validated. It fails with ErrSourceExists while name has a config
// entry, with ErrSourceBusy while an Add for a since-removed name is still in
// flight, and with ErrSourceRemovedConcurrently when a Remove took
// the name before the source finished starting. A config persist failure is
// logged, not returned.
func (l *SourceLifecycle) Add(name string, ref taskset.Ref) error {
	entry := &taskset.Entry{Ref: &ref}
	id := taskset.SourceID(name, &ref)

	l.mu.Lock()
	err := l.cfg.update(func(cfg *config.Config) error {
		if cfg.Spec.Entries == nil {
			cfg.Spec.Entries = make(map[string]*taskset.Entry)
		}
		if _, exists := cfg.Spec.Entries[name]; exists {
			return ErrSourceExists
		}
		if l.adding[name] {
			return ErrSourceBusy
		}
		cfg.Spec.Entries[name] = entry
		return nil
	})
	if errors.Is(err, ErrSourceExists) || errors.Is(err, ErrSourceBusy) {
		l.mu.Unlock()
		return err
	}
	l.adding[name] = true
	l.mu.Unlock()
	if err != nil {
		l.log.Warn("source persist failed", zap.Error(err))
	}

	ts := l.newSource(id, name, entry)
	startErr := l.rec.AddSource(ts)

	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.adding, name)
	if startErr != nil {
		if rbErr := l.cfg.update(func(cfg *config.Config) error {
			if cfg.Spec.Entries[name] == entry {
				delete(cfg.Spec.Entries, name)
			}
			return nil
		}); rbErr != nil {
			l.log.Warn("source claim rollback failed", zap.String("name", name), zap.Error(rbErr))
		}
		return fmt.Errorf("%w: %w", ErrSourceStart, startErr)
	}
	if !l.holdsClaim(name, entry) {
		l.rec.RemoveSource(id)
		l.log.Info("source removed while being added; stopped it",
			zap.String("name", name), zap.String("id", id))
		return ErrSourceRemovedConcurrently
	}
	l.reg.Register(name, ts)
	l.log.Info("source added", zap.String("name", name))
	return nil
}

// Remove deletes name's config entry, stops its source, and unregisters it.
// The registered source is stopped too, since a raw config save may have
// changed the entry's ref without restarting it, and its dev-mode sessions
// are ended so their clones do not outlive it. A config persist failure is
// logged, not returned.
func (l *SourceLifecycle) Remove(name string) error {
	src, err := l.remove(name)
	if err != nil {
		return err
	}
	if src != nil && src.DevMode() {
		src.DisableDevMode()
	}
	l.log.Info("source removed", zap.String("name", name))
	return nil
}

func (l *SourceLifecycle) remove(name string) (*taskset.Source, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	var entry *taskset.Entry
	err := l.cfg.update(func(cfg *config.Config) error {
		entry = cfg.Spec.Entries[name]
		if entry == nil {
			return ErrSourceNotFound
		}
		delete(cfg.Spec.Entries, name)
		return nil
	})
	if entry == nil {
		return nil, ErrSourceNotFound
	}
	if err != nil {
		l.log.Warn("source persist failed", zap.Error(err))
	}
	var entryID string
	if entry.Ref != nil {
		entryID = taskset.SourceID(name, entry.Ref)
		l.rec.RemoveSource(entryID)
	}
	src := l.reg.Unregister(name)
	if src != nil && src.ID() != entryID {
		l.rec.RemoveSource(src.ID())
	}
	return src, nil
}

func (l *SourceLifecycle) holdsClaim(name string, entry *taskset.Entry) bool {
	var current *taskset.Entry
	l.cfg.view(func(cfg *config.Config) { current = cfg.Spec.Entries[name] })
	return current == entry
}

func (l *SourceLifecycle) newSource(id, name string, entry *taskset.Entry) *taskset.Source {
	var opts []taskset.SourceOption
	l.cfg.view(func(cfg *config.Config) {
		opts = []taskset.SourceOption{
			taskset.WithAllowedTokenEnvs(cfg.SourceSecurity.AllowedTokenEnvs),
			taskset.WithDefaultsOnFailureChain(cfg.Defaults.OnFailureChain),
		}
	})
	if entry.Overrides != nil {
		opts = append(opts, taskset.WithParentOverrides(entry.Overrides))
	}
	return taskset.NewSource(id, name, entry.Ref, "", l.dataDir, false, entry.Ref.PollInterval, l.log, opts...)
}

// serverSourceConfig backs sourceConfigStore with the Server's cfg and cfgMu.
type serverSourceConfig struct{ s *Server }

func (c serverSourceConfig) update(mutate func(*config.Config) error) error {
	return c.s.updateConfig(mutate)
}

func (c serverSourceConfig) view(read func(*config.Config)) {
	c.s.cfgMu.RLock()
	defer c.s.cfgMu.RUnlock()
	read(c.s.cfg)
}

// serverSourceReconciler forwards to s.reconciler, read per call; with no
// reconciler wired, AddSource succeeds without starting anything.
type serverSourceReconciler struct{ s *Server }

func (r serverSourceReconciler) AddSource(src source.Source) error {
	if r.s.reconciler == nil {
		return nil
	}
	return r.s.reconciler.AddSource(src)
}

func (r serverSourceReconciler) RemoveSource(id string) {
	if r.s.reconciler != nil {
		r.s.reconciler.RemoveSource(id)
	}
}

// serverSourceRegistry forwards to s.sourceMgr, read per call; a no-op when
// no SourceManager is wired.
type serverSourceRegistry struct{ s *Server }

func (r serverSourceRegistry) Register(name string, src *taskset.Source) {
	if r.s.sourceMgr != nil {
		r.s.sourceMgr.Register(name, src)
	}
}

func (r serverSourceRegistry) Unregister(name string) *taskset.Source {
	if r.s.sourceMgr == nil {
		return nil
	}
	return r.s.sourceMgr.Unregister(name)
}

// sourceLifecycle returns the Server's SourceLifecycle, built on first use.
func (s *Server) sourceLifecycle() *SourceLifecycle {
	s.sourcesOnce.Do(func() {
		s.sources = newSourceLifecycle(serverSourceConfig{s}, serverSourceReconciler{s}, serverSourceRegistry{s}, s.dataDir, s.log)
	})
	return s.sources
}
