package taskset

import (
	"fmt"

	"github.com/dicode/dicode/pkg/task"
)

// CrossTaskWarning is an advisory about a config that only resolves across
// tasks. TaskID is the task the warning is attributed to.
type CrossTaskWarning struct {
	TaskID  string
	Message string
}

// CrossTaskParamWarnings reports required params with no default that a
// dispatch path can never satisfy, where the dispatched task is a different
// task from the one that configures the dispatch:
//   - stages of a cron-triggered pipeline (fired without fire-time params,
//     only the stage's overrides.params patch defaults);
//   - on_failure_chain / defaults.on_failure_chain targets (fired with input
//     only, never params).
//
// Targets absent from results are skipped; they may live in another source.
func CrossTaskParamWarnings(results []*ResolvedTask, defaultChain task.OnFailureChainSpec) []CrossTaskWarning {
	specs := make(map[string]*task.Spec, len(results))
	for _, rt := range results {
		if sp, ok := rt.Kinded.(*task.Spec); ok {
			specs[rt.ID] = sp
		}
	}

	var out []CrossTaskWarning
	defaultReported := false
	for _, rt := range results {
		switch k := rt.Kinded.(type) {
		case *task.PipelineTask:
			if k.Trigger.Cron == "" {
				continue
			}
			for i, st := range k.Stages {
				target, ok := specs[st.Task]
				if !ok {
					continue
				}
				effective := target.Params
				if st.Overrides != nil && len(st.Overrides.Params) > 0 {
					effective = task.ApplyParamOverridePatch(effective, st.Overrides.Params)
				}
				for _, name := range task.MissingRequiredParams(effective, nil) {
					out = append(out, CrossTaskWarning{rt.ID, fmt.Sprintf(
						"stages[%d] (%s): params.%s is required with no default, but a cron-triggered pipeline never supplies fire-time params — every fire will fail preflight (params_invalid); set it in the stage's overrides.params or give it a default",
						i, st.Task, name)})
				}
			}
		case *task.Spec:
			if k.OnFailureChain != nil {
				out = append(out, failureChainWarnings(rt.ID, "on_failure_chain", k.OnFailureChain.Task, rt.ID, specs)...)
				continue
			}
			if defaultReported {
				continue
			}
			if w := failureChainWarnings(defaultChain.Task, "defaults.on_failure_chain", defaultChain.Task, rt.ID, specs); len(w) > 0 {
				// The default target is the same for every task; report it once.
				out = append(out, w...)
				defaultReported = true
			}
		}
	}
	return out
}

func failureChainWarnings(attributeTo, field, targetID, failingID string, specs map[string]*task.Spec) []CrossTaskWarning {
	if targetID == "" || targetID == failingID {
		return nil
	}
	target, ok := specs[targetID]
	if !ok {
		return nil
	}
	var out []CrossTaskWarning
	for _, name := range task.MissingRequiredParams(target.Params, nil) {
		out = append(out, CrossTaskWarning{attributeTo, fmt.Sprintf(
			"%s target %q: params.%s is required with no default, but a failure chain never supplies fire-time params — every chained fire will fail preflight (params_invalid)",
			field, targetID, name)})
	}
	return out
}
