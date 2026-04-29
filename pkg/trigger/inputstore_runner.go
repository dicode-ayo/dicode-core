package trigger

import (
	"context"
	"fmt"

	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
)

// inputStoreTaskRunner adapts the engine's fireSync to registry.TaskRunner.
// It is constructed on demand by inputStoreTaskRunner.RunTaskSync and lets
// InputStore delegate byte-level storage to a configured storage task without
// a circular import between pkg/registry and pkg/trigger.
type inputStoreTaskRunner struct{ e *Engine }

// RunTaskSync satisfies registry.TaskRunner. It finds the named task in the
// registry, runs it synchronously via the engine's fireSync path, and returns
// the result value. Source is "input-storage" so these sub-runs are
// distinguishable in the run log.
func (r *inputStoreTaskRunner) RunTaskSync(ctx context.Context, taskID string, params map[string]string) (any, error) {
	spec, ok := r.e.registry.Get(taskID)
	if !ok {
		return nil, fmt.Errorf("storage task %q not registered", taskID)
	}
	_, result, err := r.e.fireSync(spec, pkgruntime.RunOptions{Params: params}, "input-storage")
	if err != nil {
		return nil, err
	}
	if result.Error != nil {
		return nil, result.Error
	}
	return result.ReturnValue, nil
}

// NewInputStoreTaskRunner returns a registry.TaskRunner backed by the engine.
// The daemon calls this after wiring the engine to construct the InputStore.
func NewInputStoreTaskRunner(e *Engine) registry.TaskRunner {
	return &inputStoreTaskRunner{e: e}
}
