package trigger

import (
	"fmt"
	"strings"

	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/taskset"
	"go.uber.org/zap"
)

// validatePipelineRefs runs the registry-aware checks for a PipelineTask:
// every stage task exists, is kind: Task (not a pipeline — v1 rule), each
// stage's override merge yields a valid spec, and the pipeline-stage graph is
// acyclic.
func (e *Engine) validatePipelineRefs(p *task.PipelineTask) error {
	for i, st := range p.Stages {
		k, ok := e.registry.GetKinded(st.Task)
		if !ok {
			return fmt.Errorf("pipeline.stages[%d]: task %q not found in registry", i, st.Task)
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

// registerPipeline validates a PipelineTask against the registry. Trigger
// scheduling (cron/webhook) and execution wiring land in a later PR; manual
// pipelines need no scheduling here.
// NOTE: registerPipeline must be called with e.registerMu already held (Engine.Register does this).
func (e *Engine) registerPipeline(p *task.PipelineTask) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if err := e.validatePipelineRefs(p); err != nil {
		return err
	}
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
