package trigger

import (
	"errors"
	"fmt"
	"strings"

	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/taskset"
	"go.uber.org/zap"
)

// errPipelineStageMissing marks a pipeline-ref validation failure caused solely
// by a stage task not yet being present in the registry (the cold-start
// ordering case, #341). registerPipeline wraps the per-stage error with this
// sentinel so Register can distinguish "defer and retry once the stage lands"
// from genuine, non-transient validation failures (bad subtype, invalid
// overrides, cycle) which must still surface as errors.
var errPipelineStageMissing = errors.New("pipeline stage task not yet registered")

// validatePipelineRefs runs the registry-aware checks for a PipelineTask:
// every stage task exists, is kind: Task (not a pipeline — v1 rule), each
// stage's override merge yields a valid spec, and the pipeline-stage graph is
// acyclic.
func (e *Engine) validatePipelineRefs(p *task.PipelineTask) error {
	for i, st := range p.Stages {
		k, ok := e.registry.GetKinded(st.Task)
		if !ok {
			// Wrap with the sentinel so registerPipeline can defer-and-retry
			// rather than permanently reject — the stage may simply not have
			// been reconciled yet (cold-start ordering, #341).
			return fmt.Errorf("pipeline.stages[%d]: task %q not found in registry: %w", i, st.Task, errPipelineStageMissing)
		}
		ref, ok := k.(*task.Spec)
		if !ok {
			return fmt.Errorf("pipeline.stages[%d]: task %q is a %s; pipeline stages must be kind: Task (v1)", i, st.Task, k.KindOf())
		}
		if st.Overrides != nil {
			// Default to the original overrides; replaced with a stripped copy below only when Params need ${input.…} cleared.
			ovPtr := st.Overrides
			// Strip ${input.…} param defaults before the merge-validate; they
			// resolve at dispatch time, not registration time.
			if st.Overrides.Params != nil {
				ovCopy := *st.Overrides
				cleaned := make(task.ParamOverrides, len(st.Overrides.Params))
				copy(cleaned, st.Overrides.Params)
				for j := range cleaned {
					if strings.Contains(cleaned[j].Default, "${input.") {
						cleaned[j].Default = ""
					}
				}
				ovCopy.Params = cleaned
				ovPtr = &ovCopy
			}
			merged := taskset.ApplyOverrides(ref, ovPtr)
			if vErr := merged.Validate(); vErr != nil {
				return fmt.Errorf("pipeline.stages[%d] (%s): overrides produce invalid spec: %w", i, st.Task, vErr)
			}
		}
	}
	if cyclePath := e.detectPipelineCycle(p); cyclePath != "" {
		return fmt.Errorf("pipeline: cycle detected: %s", cyclePath)
	}
	return nil
}

// detectPipelineCycle runs a three-colour DFS over the pipeline-stage graph
// overlaid on the registry (mirrors detectBeforeCycle, edges are
// PipelineTask -> Stage.Task). Returns a printable path or "".
// Currently unreachable in v1 (stages are kind: Task only, so pipeline->pipeline edges can't form); retained for when nested pipelines land.
func (e *Engine) detectPipelineCycle(p *task.PipelineTask) string {
	edges := map[string][]string{}
	for _, k := range e.registry.AllKinded() {
		pt, ok := k.(*task.PipelineTask)
		if !ok || pt.ID == p.ID {
			continue
		}
		out := make([]string, 0, len(pt.Stages))
		for _, st := range pt.Stages {
			out = append(out, st.Task)
		}
		edges[pt.ID] = out
	}
	out := make([]string, 0, len(p.Stages))
	for _, st := range p.Stages {
		out = append(out, st.Task)
	}
	edges[p.ID] = out

	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var stack []string
	var dfs func(id string) string
	dfs = func(id string) string {
		color[id] = gray
		stack = append(stack, id)
		for _, next := range edges[id] {
			switch color[next] {
			case gray:
				start := 0
				for idx, n := range stack {
					if n == next {
						start = idx
						break
					}
				}
				path := append([]string(nil), stack[start:]...)
				path = append(path, next)
				return strings.Join(path, " -> ")
			case white:
				if cp := dfs(next); cp != "" {
					return cp
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
		return ""
	}
	return dfs(p.ID)
}

// registerPipeline validates a PipelineTask against the registry and schedules
// its cron/webhook triggers. Manual pipelines fire via FireManual and chain
// pipelines via FireChain, so neither needs scheduling here.
//
// Cold-start ordering (#341): if validation fails ONLY because a stage task is
// not yet registered (errPipelineStageMissing), the pipeline is recorded in
// deferredPipelines and registerPipeline returns nil — it will be retried when
// a kind: Task later registers (see retryDeferredPipelines). Any other
// validation failure (bad subtype, invalid overrides, cycle) still returns an
// error so the operator sees the misconfiguration.
//
// NOTE: registerPipeline must be called with e.registerMu already held (Engine.Register does this),
// which is also what guards deferredPipelines.
func (e *Engine) registerPipeline(p *task.PipelineTask) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if err := e.validatePipelineRefs(p); err != nil {
		if errors.Is(err, errPipelineStageMissing) {
			// Defer: keep the pipeline so a later kind: Task registration can
			// heal it without a file change. Logged once at INFO (not WARN) so
			// reconcile cycles don't spam — a permanently-deferred pipeline is
			// the bug, a transiently-deferred one is expected at cold start.
			if _, already := e.deferredPipelines[p.ID]; !already {
				e.log.Info("pipeline waiting for stage tasks to register — will retry",
					zap.String("task", p.ID), zap.Error(err))
			}
			e.deferredPipelines[p.ID] = p
			return nil
		}
		return err
	}
	// Validated cleanly — it must not linger in the deferred set (e.g. a
	// previously-deferred pipeline whose stages have now arrived, or a
	// re-registration after an edit).
	delete(e.deferredPipelines, p.ID)
	if !p.Enabled {
		e.log.Info("pipeline registered (disabled — no triggers scheduled)", zap.String("task", p.ID))
		return nil
	}
	// Schedule cron/webhook triggers. Manual pipelines fire via FireManual and
	// chain pipelines via FireChain (Task 16), so neither needs scheduling here.
	if p.Trigger.Cron != "" {
		e.registerPipelineCron(p)
	}
	if p.Trigger.Webhook != "" {
		e.registerWebhookPath(p.ID, p.Trigger.Webhook)
	}
	e.log.Info("pipeline registered", zap.String("task", p.ID), zap.Int("stages", len(p.Stages)))
	return nil
}

// retryDeferredPipelines re-attempts registration of every pipeline currently
// parked in deferredPipelines. Called after a kind: Task registers
// successfully — the newly-arrived stage may be exactly the one a deferred
// pipeline was waiting for. Each retry runs registerPipeline, which removes the
// pipeline from the set on success (it schedules cron/webhook) and leaves it
// parked if a stage is still missing. A genuinely-broken pipeline that now
// fails for a NON-missing-stage reason (e.g. an override that becomes invalid
// once its stage exists) is dropped from the deferred set and surfaces a WARN —
// it will never schedule but also won't be retried forever.
//
// MUST be called with e.registerMu held (Register holds it across the whole
// path); deferredPipelines is guarded by registerMu.
func (e *Engine) retryDeferredPipelines() {
	if len(e.deferredPipelines) == 0 {
		return
	}
	// Snapshot the IDs so we can mutate the map inside the loop (registerPipeline
	// deletes on success). Order is irrelevant: each pipeline validates against
	// the full registry independently.
	pending := make([]*task.PipelineTask, 0, len(e.deferredPipelines))
	for _, p := range e.deferredPipelines {
		pending = append(pending, p)
	}
	for _, p := range pending {
		// A pipeline may have been unregistered/superseded between defer and
		// retry; skip if it's no longer the parked entry for this ID.
		if e.deferredPipelines[p.ID] != p {
			continue
		}
		if err := e.registerPipeline(p); err != nil {
			// registerPipeline returns nil for the still-missing-stage case
			// (re-defers). A non-nil error here means the pipeline is genuinely
			// invalid now that its stage exists — stop retrying it.
			delete(e.deferredPipelines, p.ID)
			e.log.Warn("deferred pipeline failed to register after stage arrived — fix the spec to re-enable",
				zap.String("task", p.ID), zap.Error(err))
		}
	}
}
