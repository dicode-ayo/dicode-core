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
type sourceRegistry interface {
	Register(name string, src *taskset.Source)
	Get(name string) (*taskset.Source, bool)
	Unregister(name string, src *taskset.Source) bool
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
type SourceLifecycle struct {
	mu      sync.Mutex
	cfg     sourceConfigStore
	rec     sourceReconciler
	reg     sourceRegistry
	dataDir string
	log     *zap.Logger
}

func newSourceLifecycle(cfg sourceConfigStore, rec sourceReconciler, reg sourceRegistry, dataDir string, log *zap.Logger) *SourceLifecycle {
	return &SourceLifecycle{cfg: cfg, rec: rec, reg: reg, dataDir: dataDir, log: log}
}

// Add claims name for ref, starts the source, and registers it. ref must
// already be validated. A config persist failure is logged, not returned.
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
		cfg.Spec.Entries[name] = entry
		return nil
	})
	l.mu.Unlock()
	if errors.Is(err, ErrSourceExists) {
		return err
	}
	if err != nil {
		l.log.Warn("source persist failed", zap.Error(err))
	}

	ts := l.newSource(id, name, entry)
	if err := l.rec.AddSource(ts); err != nil {
		if rbErr := l.cfg.update(func(cfg *config.Config) error {
			if cfg.Spec.Entries[name] == entry {
				delete(cfg.Spec.Entries, name)
			}
			return nil
		}); rbErr != nil {
			l.log.Warn("source claim rollback failed", zap.String("name", name), zap.Error(rbErr))
		}
		return fmt.Errorf("%w: %w", ErrSourceStart, err)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.holdsClaim(name, entry) {
		l.rec.RemoveSource(id)
		l.log.Info("source removed or replaced while being added; stopped it",
			zap.String("name", name), zap.String("id", id))
		return nil
	}
	l.reg.Register(name, ts)
	l.log.Info("source added", zap.String("name", name))
	return nil
}

// Remove deletes name's config entry, stops its source, and unregisters it.
// A config persist failure is logged, not returned.
func (l *SourceLifecycle) Remove(name string) error {
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
		return ErrSourceNotFound
	}
	if err != nil {
		l.log.Warn("source persist failed", zap.Error(err))
	}
	if entry.Ref != nil {
		l.rec.RemoveSource(taskset.SourceID(name, entry.Ref))
	}
	if src, ok := l.reg.Get(name); ok {
		l.reg.Unregister(name, src)
	}
	l.log.Info("source removed", zap.String("name", name))
	return nil
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

func (r serverSourceRegistry) Get(name string) (*taskset.Source, bool) {
	if r.s.sourceMgr == nil {
		return nil, false
	}
	return r.s.sourceMgr.Get(name)
}

func (r serverSourceRegistry) Unregister(name string, src *taskset.Source) bool {
	if r.s.sourceMgr == nil {
		return false
	}
	return r.s.sourceMgr.Unregister(name, src)
}

// sourceLifecycle returns the Server's SourceLifecycle, built on first use.
func (s *Server) sourceLifecycle() *SourceLifecycle {
	s.sourcesOnce.Do(func() {
		s.sources = newSourceLifecycle(serverSourceConfig{s}, serverSourceReconciler{s}, serverSourceRegistry{s}, s.dataDir, s.log)
	})
	return s.sources
}
