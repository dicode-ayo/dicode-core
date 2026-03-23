package registry

import (
	"context"
	"fmt"

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
}

// NewReconciler creates a Reconciler for the given registry and sources.
func NewReconciler(r *Registry, sources []source.Source, log *zap.Logger) *Reconciler {
	return &Reconciler{registry: r, sources: sources, log: log}
}

// Run starts all sources and processes their events until ctx is cancelled.
func (rc *Reconciler) Run(ctx context.Context) error {
	if len(rc.sources) == 0 {
		<-ctx.Done()
		return nil
	}

	// Fan-in: merge all source channels into one.
	merged := make(chan source.Event, 64)
	for _, src := range rc.sources {
		ch, err := src.Start(ctx)
		if err != nil {
			return fmt.Errorf("start source %s: %w", src.ID(), err)
		}
		go func(c <-chan source.Event) {
			for ev := range c {
				merged <- ev
			}
		}(ch)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-merged:
			rc.handle(ev)
		}
	}
}

func (rc *Reconciler) handle(ev source.Event) {
	switch ev.Kind {
	case source.EventAdded, source.EventUpdated:
		spec, err := task.LoadDir(ev.TaskDir)
		if err != nil {
			rc.log.Warn("failed to load task",
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
