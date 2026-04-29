package registry

import (
	"context"
	"fmt"
	"sync"

	"github.com/dicode/dicode/pkg/source"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// Reconciler fans-in events from multiple sources and applies them to the registry.
type Reconciler struct {
	registry *Registry
	sources  []source.Source
	dataDir  string
	log      *zap.Logger

	// OnRegister is called after a task is registered (used by trigger engine).
	OnRegister func(spec *task.Spec)
	// OnUnregister is called after a task is removed.
	OnUnregister func(id string)

	// runtime state — set when Run is called
	mu      sync.Mutex
	merged  chan source.Event
	cancels map[string]context.CancelFunc // sourceID → cancel fn
	runCtx  context.Context
}

// NewReconciler creates a Reconciler for the given registry and sources.
// dataDir is the daemon's data directory (config.DataDir); it is injected as
// the ${DATADIR} template variable when loading task specs so buildin tasks
// can reference shared paths under the data dir.
func NewReconciler(r *Registry, sources []source.Source, dataDir string, log *zap.Logger) *Reconciler {
	return &Reconciler{
		registry: r,
		sources:  sources,
		dataDir:  dataDir,
		log:      log,
		cancels:  make(map[string]context.CancelFunc),
	}
}

// Run starts all sources and processes their events until ctx is cancelled.
func (rc *Reconciler) Run(ctx context.Context) error {
	rc.mu.Lock()
	rc.runCtx = ctx
	rc.merged = make(chan source.Event, 64)
	rc.mu.Unlock()

	if len(rc.sources) == 0 {
		// Still need to run so dynamic AddSource works.
		goto loop
	}

	for _, src := range rc.sources {
		if err := rc.startSource(src); err != nil {
			return fmt.Errorf("start source %s: %w", src.ID(), err)
		}
	}

loop:
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-rc.merged:
			rc.handle(ev)
		}
	}
}

// AddSource adds a new source at runtime and starts it immediately.
// Safe to call from any goroutine after Run has been called.
func (rc *Reconciler) AddSource(src source.Source) error {
	return rc.startSource(src)
}

// RemoveSource stops and removes a source by its ID.
// Safe to call from any goroutine after Run has been called.
func (rc *Reconciler) RemoveSource(id string) {
	rc.mu.Lock()
	cancel, ok := rc.cancels[id]
	delete(rc.cancels, id)
	rc.mu.Unlock()
	if ok {
		cancel()
	}
}

// startSource begins watching a source and forwarding its events to merged.
func (rc *Reconciler) startSource(src source.Source) error {
	rc.mu.Lock()
	ctx := rc.runCtx
	rc.mu.Unlock()
	if ctx == nil {
		return fmt.Errorf("reconciler not yet running")
	}

	srcCtx, cancel := context.WithCancel(ctx)
	ch, err := src.Start(srcCtx)
	if err != nil {
		cancel()
		return err
	}

	rc.mu.Lock()
	rc.cancels[src.ID()] = cancel
	rc.mu.Unlock()

	go func() {
		for ev := range ch {
			// Without the ctx select, a slow main loop plus a closed merged
			// reader during shutdown would block this goroutine forever
			// holding source events. ctx is the parent context, not the
			// per-source srcCtx, so this drains naturally on full reconciler
			// shutdown rather than per-source cancellation.
			select {
			case rc.merged <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return nil
}

func (rc *Reconciler) handle(ev source.Event) {
	switch ev.Kind {
	case source.EventAdded, source.EventUpdated:
		var spec *task.Spec
		if ev.Spec != nil {
			// TaskSet sources pre-resolve the spec (overrides already applied).
			spec = ev.Spec
			spec.ID = ev.TaskID
		} else {
			var err error
			extras := ev.ExtraVars
			if extras == nil {
				extras = make(map[string]string, 1)
			}
			if _, ok := extras[task.VarDataDir]; !ok && rc.dataDir != "" {
				// Don't clobber a source-supplied DATADIR (allows tests to override).
				// Clone before mutate — ev.ExtraVars may be shared across event consumers.
				cloned := make(map[string]string, len(extras)+1)
				for k, v := range extras {
					cloned[k] = v
				}
				cloned[task.VarDataDir] = rc.dataDir
				extras = cloned
			}
			spec, err = task.LoadDirWithVars(ev.TaskDir, extras)
			if err != nil {
				rc.log.Warn("failed to load task",
					zap.String("task", ev.TaskID),
					zap.String("source", ev.Source),
					zap.Error(err),
				)
				return
			}
		}
		if err := rc.validateTaskProviders(spec); err != nil {
			rc.log.Warn("task references unknown provider",
				zap.String("task", ev.TaskID),
				zap.String("source", ev.Source),
				zap.Error(err),
			)
			return
		}
		if err := rc.registry.Register(spec); err != nil {
			rc.log.Error("failed to register task", zap.String("task", ev.TaskID), zap.Error(err))
			return
		}
		rc.log.Info("task registered",
			zap.String("task", ev.TaskID),
			zap.String("kind", string(ev.Kind)),
		)
		if rc.OnRegister != nil {
			rc.OnRegister(spec)
		}

	case source.EventRemoved:
		rc.registry.Unregister(ev.TaskID)
		rc.log.Info("task unregistered", zap.String("task", ev.TaskID))
		if rc.OnUnregister != nil {
			rc.OnUnregister(ev.TaskID)
		}
	}
}

// validateTaskProviders inspects every EnvEntry whose From has the
// "task:" prefix and confirms the referenced provider task is already
// registered. Issue #119: a misspelled provider must not silently fall
// through to a runtime spawn failure on every consumer launch.
//
// Order dependency: provider tasks must reconcile before their consumers.
// The buildin source registers providers first because they live under
// tasks/buildin/secret-providers/* and the taskset.yaml entry order is
// preserved. For multi-source setups, a transient miss on first
// reconciler pass causes the consumer to be skipped; the next sync (30s
// later, or on the source's next event) retries.
func (rc *Reconciler) validateTaskProviders(spec *task.Spec) error {
	for _, e := range spec.Permissions.Env {
		kind, target := task.ParseFrom(e.From)
		if kind != task.FromKindTask {
			continue
		}
		if target == "" {
			return fmt.Errorf("env entry %q: from: task: target is empty", e.Name)
		}
		if _, ok := rc.registry.Get(target); !ok {
			return fmt.Errorf("env entry %q: provider task %q not registered", e.Name, target)
		}
	}
	return nil
}
