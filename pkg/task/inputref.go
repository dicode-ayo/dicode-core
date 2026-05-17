// Package task: dispatch-time interpolation for per-edge override params.
//
// pkg/task/template.go handles spec-load-time template variables
// (${DATADIR}, ${TASK_DIR}, …). Those resolve when the reconciler
// reads task.yaml off disk.
//
// ResolveInputOutput{Map,List} are the dispatch-time complement: when
// a downstream task is fired (via chain.from or via trigger.before
// pipeline), any of its params whose VALUE is exactly "${input.output}"
// is replaced with the upstream's return value.
//
// Two shapes are supported because the two call sites use different
// param containers:
//
//   - trigger.chain.params is map[string]any (values may be string,
//     number, bool, list). ResolveInputOutputMap walks string values.
//
//   - trigger.before[].overrides.params is ParamOverrides
//     ([]ParamOverride). The Default string field carries the value.
//     ResolveInputOutputList walks Default fields.
//
// Both share InputOutputToken + ErrInputUnavailable.
//
// Narrow grammar by design: only the exact token form is recognised.
// Embedded references ("prefix-${input.output}-x") and other tokens
// ("${input.params.foo}") remain literal. Future extensions can grow
// the grammar without breaking the existing single-token contract.

package task

import "fmt"

// InputOutputToken is the literal string the resolver looks for.
const InputOutputToken = "${input.output}"

// ErrInputUnavailable is returned when a param references
// ${input.output} but no upstream return value is available (e.g. the
// first stage of a trigger.before pipeline, or a chain dispatch where
// the upstream produced nothing or produced a non-string return value).
type ErrInputUnavailable struct {
	Param string // name of the offending param
}

func (e *ErrInputUnavailable) Error() string {
	return fmt.Sprintf("param %q references ${input.output} but no upstream return value is available", e.Param)
}

// ResolveInputOutputMap returns a NEW params map with the
// "${input.output}" token substituted by upstreamOutput on every
// string-typed value matching the token exactly. Non-string values
// and string values containing other content are copied through
// unchanged.
//
// If upstreamOutput is "" and the token appears in a string value,
// returns *ErrInputUnavailable identifying one of the offending
// params (Go map iteration order is undefined; the loud failure
// is what matters, not the specific Param name).
//
// The input map is NOT mutated.
func ResolveInputOutputMap(params map[string]any, upstreamOutput string) (map[string]any, error) {
	if params == nil {
		return nil, nil
	}
	out := make(map[string]any, len(params))
	for k, v := range params {
		s, ok := v.(string)
		if !ok || s != InputOutputToken {
			out[k] = v
			continue
		}
		if upstreamOutput == "" {
			return nil, &ErrInputUnavailable{Param: k}
		}
		out[k] = upstreamOutput
	}
	return out, nil
}

// ResolveInputOutputList returns a NEW ParamOverrides slice with
// "${input.output}" Default values substituted by upstreamOutput.
// Other Default values are copied through unchanged.
//
// If upstreamOutput is "" and the token appears, returns
// *ErrInputUnavailable identifying the offending entry.
//
// The input slice is NOT mutated; each ParamOverride is copied.
func ResolveInputOutputList(params ParamOverrides, upstreamOutput string) (ParamOverrides, error) {
	if params == nil {
		return nil, nil
	}
	out := make(ParamOverrides, len(params))
	for i, p := range params {
		out[i] = p // copy by value — ParamOverride is a small struct
		if p.Default != InputOutputToken {
			continue
		}
		if upstreamOutput == "" {
			return nil, &ErrInputUnavailable{Param: p.Name}
		}
		out[i].Default = upstreamOutput
	}
	return out, nil
}
