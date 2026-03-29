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
func NewReconciler(r *Registry, sources []source.Source, log *zap.Logger) *Reconciler {
	return &Reconciler{
		registry: r,
		sources:  sources,
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
			rc.merged <- ev
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
			spec, err = task.LoadDir(ev.TaskDir)
			if err != nil {
				rc.log.Warn("failed to load task",
					zap.String("task", ev.TaskID),
					zap.String("source", ev.Source),
					zap.Error(err),
				)
				return
			}
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
