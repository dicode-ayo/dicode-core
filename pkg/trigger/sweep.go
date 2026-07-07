package trigger

import (
	"context"

	"github.com/dicode/dicode/pkg/registry"
	"go.uber.org/zap"
)

// SweepExpiredSuspensions cancels suspended runs past their resume deadline via
// the registry sweep, then drives the finish side-effects a normal cancellation
// performs for each run the sweep actually transitioned: the run:finished hook
// (so a live run-detail/form page learns the suspension expired instead of
// waiting on a stale resume form) and FireChain on the resume_timeout
// cancellation (so on:always / on:failure chain edges observe the outcome).
// Returns the IDs of the cancelled runs.
//
// The registry sweep returns only runs whose status-guarded UPDATE transitioned
// suspended→cancelled, so a run that resumed between the sweep's SELECT and its
// UPDATE is not finished twice here. Side-effects run on context.Background so a
// cancelled sweep context (daemon shutdown) can't drop the matching finish,
// mirroring the engine's other finish-hook call sites.
func (e *Engine) SweepExpiredSuspensions(ctx context.Context, nowMs int64) ([]string, error) {
	swept, err := e.registry.SweepExpiredSuspensions(ctx, nowMs)
	if err != nil {
		return nil, err
	}
	for _, runID := range swept {
		run, gerr := e.registry.GetRun(context.Background(), runID)
		if gerr != nil {
			e.log.Warn("sweep: load cancelled suspension for finish hooks failed",
				zap.String("run", runID), zap.Error(gerr))
			continue
		}
		if h := e.runFinishedHook; h != nil {
			// Duration 0: the suspended body isn't re-executed on timeout.
			h(run.TaskID, run.ID, registry.StatusCancelled, string(run.TriggerSource), 0)
		}
		e.FireChain(context.Background(), run.TaskID, run.ID, registry.StatusCancelled, nil, nil)
	}
	return swept, nil
}
