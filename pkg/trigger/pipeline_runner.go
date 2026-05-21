package trigger

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/taskset"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// PipelineRunner orchestrates one fire of a kind: PipelineTask. It owns the
// pipeline's parent run row and drives its stages sequentially. One runner per
// fire; it holds no state beyond the in-flight pipeline.
type PipelineRunner struct {
	engine *Engine
	spec   *task.PipelineTask
	runID  string // the pipeline's own parent run ID
}

// firePipeline creates the pipeline's parent run row (kind=pipeline) and starts
// the runner asynchronously, returning the parent run ID immediately (mirrors
// fireAsync's contract for kind: Task).
//
// It re-validates the pipeline against the live registry before creating any
// run row. Manual and chain fires reach this path via GetKinded without going
// through engine registration, so a pipeline whose stages were deregistered (or
// that lost the reconcile-ordering race, see #341) is rejected loudly here
// rather than dispatched and failed mid-flight.
func (e *Engine) firePipeline(ctx context.Context, p *task.PipelineTask, opts pkgruntime.RunOptions, source registry.TriggerSource) (string, error) {
	if err := e.validatePipelineRefs(p); err != nil {
		return "", fmt.Errorf("pipeline %q failed validation: %w", p.ID, err)
	}
	runID := uuid.New().String()
	if _, err := e.registry.StartRunWithID(context.Background(), runID, p.ID, opts.ParentRunID, string(source), registry.RunKindPipeline); err != nil {
		return "", fmt.Errorf("start pipeline run: %w", err)
	}
	r := &PipelineRunner{engine: e, spec: p, runID: runID}
	go r.run(ctx)
	return runID, nil
}

// run executes stages sequentially, threading each stage's output into the next
// via ${input.*}, and short-circuits the pipeline on the first stage failure.
func (r *PipelineRunner) run(ctx context.Context) {
	e := r.engine
	if r.spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.spec.Timeout)
		defer cancel()
	}

	var upstream task.InputContext
	var lastReturn interface{}
	for i, st := range r.spec.Stages {
		out, err := e.dispatchStage(ctx, st, upstream, r.runID)
		if err != nil {
			reason := fmt.Sprintf("stage %d (%s): %s", i, st.Task, err.Error())
			e.log.Warn("pipeline stage failed; pipeline failed",
				zap.String("pipeline", r.spec.ID), zap.Int("stage", i),
				zap.String("task", st.Task), zap.Error(err))
			r.finish(registry.StatusFailure, reason, nil)
			return
		}
		upstream = out
		lastReturn = out.Output
	}
	r.finish(registry.StatusSuccess, "", lastReturn)
}

// dispatchStage resolves the stage's overrides + ${input.*} references against
// the upstream stage's output, fires the stage as a kind: Task child of the
// pipeline run, and blocks until it reaches a terminal state. It returns the
// stage's InputContext for the next stage.
func (e *Engine) dispatchStage(ctx context.Context, st task.Stage, upstream task.InputContext, parentRunID string) (task.InputContext, error) {
	ref, ok := e.registry.Get(st.Task) // Get filters to kind: Task — stages are always Task
	if !ok {
		return task.InputContext{}, fmt.Errorf("stage task %q not registered", st.Task)
	}

	dispatchSpec := ref
	if st.Overrides != nil {
		ovPtr := st.Overrides
		if st.Overrides.Params != nil {
			resolved, rerr := task.ResolveInputOutputList(st.Overrides.Params, upstream)
			if rerr != nil {
				return task.InputContext{}, fmt.Errorf("resolve input refs: %w", rerr)
			}
			ovCopy := *st.Overrides
			ovCopy.Params = resolved
			ovPtr = &ovCopy
		}
		merged := taskset.ApplyOverrides(ref, ovPtr)
		if vErr := merged.Validate(); vErr != nil {
			return task.InputContext{}, fmt.Errorf("overrides produce invalid spec: %w", vErr)
		}
		dispatchSpec = merged
	}

	runID, err := e.fireAsync(ctx, dispatchSpec, pkgruntime.RunOptions{ParentRunID: parentRunID}, registry.TriggerPipelineStage)
	if err != nil {
		return task.InputContext{}, fmt.Errorf("dispatch: %w", err)
	}
	res, werr := e.WaitRun(ctx, runID)
	if werr != nil {
		return task.InputContext{}, fmt.Errorf("wait: %w", werr)
	}
	if res.Status != registry.StatusSuccess {
		return task.InputContext{}, fmt.Errorf("run %s ended with status %s", runID, res.Status)
	}
	return task.InputContext{Output: res.ReturnValue}, nil
}

// finish marks the pipeline's parent run terminal and records the terminal
// stage's return value as the pipeline's own return (persisted to the run row,
// so chain consumers and WaitRun observe it — mirrors how runTask persists).
func (r *PipelineRunner) finish(status, reason string, ret interface{}) {
	e := r.engine
	finishCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if ret != nil {
		if b, err := json.Marshal(ret); err == nil {
			_ = e.registry.SetRunResult(finishCtx, r.runID, string(b), "", "")
		}
	}
	if reason != "" {
		_ = e.registry.FinishRunWithReason(finishCtx, r.runID, status, reason)
	} else {
		_ = e.registry.FinishRun(finishCtx, r.runID, status)
	}
}
