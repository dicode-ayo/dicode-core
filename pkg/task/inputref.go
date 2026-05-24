// Package task: dispatch-time interpolation for per-edge override params.
//
// pkg/task/template.go handles spec-load-time template variables
// (${DATADIR}, ${TASK_DIR}, …). Those resolve when the reconciler
// reads task.yaml off disk.
//
// ResolveInputOutput{Map,List} are the dispatch-time complement: when
// a downstream task is fired (via chain.from or via a kind: PipelineTask
// stage), any of its params whose VALUE contains a recognised
// `${input.…}` token is rewritten in place. Three shapes are
// recognised:
//
//   - ${input.output}            — the upstream's full string return
//     value. Non-string upstream → ErrInputUnavailable.
//
//   - ${input.output.<field>}    — a named field from an upstream that
//     returned a structured (map-shaped) value. Field must exist and
//     hold a string value; otherwise ErrInputUnavailable.
//
//   - ${input.params.<name>}     — a named param from the upstream's
//     RunOptions.Params at dispatch time. Pipes the original caller-
//     supplied param value into the downstream. Missing → ErrInputUnavailable.
//
// Embedded forms ("prefix-${input.output}-x") and multi-token forms
// ("${input.params.scheme}://${input.output.path}") ARE supported —
// any string containing one or more recognised tokens is rewritten
// through ReplaceAllStringFunc. Strings without `${input.` short-circuit
// and pass through literally with no allocation.
//
// Two shapes are supported because the two call sites use different
// param containers:
//
//   - trigger.chain.params is map[string]any (values may be string,
//     number, bool, list). ResolveInputOutputMap walks string values.
//
//   - pipeline stage overrides.params (kind: PipelineTask) is
//     ParamOverrides ([]ParamOverride). The Default string field carries
//     the value. ResolveInputOutputList walks Default fields.
//
// Both share InputContext + ErrInputUnavailable + ValidateInputRefs.
//
// Unknown shapes (e.g. `${input.foo}`, `${input.output.dotted.path}`,
// `${input.params}` without a field name) are rejected loudly:
//   - at registration time via ValidateInputRefs (the static path)
//   - at dispatch time via ErrInputUnavailable (the runtime backstop,
//     if a config skips the registration-time check)

package task

import (
	"fmt"
	"regexp"
	"strings"
)

// inputRefRe matches the three recognised reference shapes:
//
//	${input.output}              → kind="output",  field=""
//	${input.output.<ident>}      → kind="output",  field=<ident>
//	${input.params.<ident>}      → kind="params",  field=<ident>
//
// <ident> is a permissive identifier-ish name shaped to fit real-world
// YAML keys and HTTP header names: a leading [A-Za-z_], followed by
// any mix of [A-Za-z0-9_], hyphens, and dots. This accepts common
// header-style names like `x-forwarded-for` and dotted keys like
// `db.host` without ambiguity — the upstream params map is itself
// indexed by arbitrary string keys (params.go), so the resolver
// shouldn't be narrower than its lookup target. The leading character
// is still restricted to a letter or underscore so that genuinely
// malformed shapes like `${input.output.0bad}` are still rejected by
// ValidateInputRefs.
//
// Anything else inside `${input.…}` (e.g. `${input.foo}`,
// `${input.params}` with no field) does NOT match the regex —
// ValidateInputRefs surfaces those as loud errors at registration time.
var inputRefRe = regexp.MustCompile(`\$\{input\.(output|params)(?:\.([A-Za-z_][A-Za-z0-9_.\-]*))?\}`)

// inputRefPrefixRe matches any `${input.…}` substring regardless of
// shape — used by ValidateInputRefs to detect interpolation attempts
// the strict regex would silently pass through.
var inputRefPrefixRe = regexp.MustCompile(`\$\{input\.[^}]*\}`)

// inputParamsRefRe matches any ${input.params.<name>} reference specifically —
// used by PipelineTask.Validate to reject ${input.params.…} references in
// sequential pipelines (where the upstream params map is not forwarded).
var inputParamsRefRe = regexp.MustCompile(`\$\{input\.params\.[A-Za-z_][A-Za-z0-9_.\-]*\}`)

// InputContext carries the dispatch-time values the resolver needs.
// Output is the upstream's full return value (any type — the resolver
// type-asserts per-token). Params is the upstream's RunOptions.Params
// snapshot at dispatch time; nil is treated as "no params available"
// and any ${input.params.X} reference returns ErrInputUnavailable.
type InputContext struct {
	Output interface{}
	Params map[string]string
}

// ErrInputUnavailable is returned when a param references an
// `${input.…}` token that cannot be resolved at dispatch time. The
// resolver treats an empty-string value as "not provided" uniformly
// across all three forms (the loud-failure contract):
//   - ${input.output} when the upstream returned a non-string or an
//     empty string
//   - ${input.output.<field>} when the upstream returned a non-map,
//     when the field is absent, when the field's value is non-string,
//     or when the field's string value is empty
//   - ${input.params.<name>} when the upstream's Params map is nil,
//     when the named entry is missing, or when the named entry's value
//     is empty
//
// Param identifies the offending param so operators can see which
// override or chain key tripped the resolver.
//
// BREAKING CHANGE (PR #336 / issue #333): previously, ${input.output.<field>}
// and ${input.params.<name>} silently substituted empty-string values without
// failing. Operators upgrading should audit any task.yaml relying on that
// loose behavior; the loud-failure contract now applies uniformly to all
// three ${input.*} forms.
type ErrInputUnavailable struct {
	Param string // name of the offending param
	Ref   string // the offending reference token, e.g. "${input.output.path}"
	Why   string // short human-readable reason ("upstream returned no string", etc.)
}

func (e *ErrInputUnavailable) Error() string {
	if e.Ref == "" && e.Why == "" {
		return fmt.Sprintf("param %q references ${input.output} but no upstream return value is available", e.Param)
	}
	if e.Why == "" {
		return fmt.Sprintf("param %q references %s but no value is available", e.Param, e.Ref)
	}
	return fmt.Sprintf("param %q references %s: %s", e.Param, e.Ref, e.Why)
}

// ValidateInputRefs walks s and surfaces any `${input.…}` substring
// that does NOT match the strict regex (e.g. `${input.foo}`,
// `${input.output.a.b}`, `${input.params}` with no field). Returns nil
// when every interpolation attempt is well-formed AND when s contains
// no interpolation attempts at all. Site is a free-form location
// string ("trigger.chain.params.X", "pipeline.stages[2].overrides.params.X")
// included in the error so operators can pinpoint the offending field
// in their task.yaml.
//
// This is the registration-time gate; the dispatch-time resolver
// (resolveString) accepts the same well-formed shapes but ALSO
// passes through literal `${input.…}` substrings that aren't proper
// `${input.output}` / `${input.params.X}` references — that's how a
// literal `$${input.output}` would have to be handled if we ever
// added escape syntax. For now, since ValidateInputRefs runs at
// registration, an unknown shape can't reach the dispatch resolver
// anyway.
func ValidateInputRefs(site, s string) error {
	if !strings.Contains(s, "${input.") {
		return nil
	}
	matches := inputRefPrefixRe.FindAllString(s, -1)
	for _, m := range matches {
		sub := inputRefRe.FindStringSubmatch(m)
		if sub == nil {
			return fmt.Errorf("%s: unknown reference shape %s (allowed: ${input.output}, ${input.output.<field>}, ${input.params.<name>})", site, m)
		}
		// ${input.params} without a named field is rejected — the
		// resolver needs a specific key. ${input.output} without a
		// field is fine (it means "the whole upstream string").
		kind, field := sub[1], sub[2]
		if kind == "params" && field == "" {
			return fmt.Errorf("%s: %s requires a named field (e.g. ${input.params.url})", site, m)
		}
	}
	return nil
}

// resolveString rewrites s by substituting every recognised
// `${input.…}` token against ctx. Strings without any `${input.`
// substring pass through with no allocation. On the first
// unresolvable token the function aborts and returns the underlying
// ErrInputUnavailable (with the supplied paramName attached).
//
// Unknown shapes (well-formed `${input.…}` substrings that don't
// match inputRefRe) are passed through unchanged — ValidateInputRefs
// catches those at registration time, so the dispatch-time resolver
// stays focused on resolving valid references.
func resolveString(paramName, s string, ctx InputContext) (string, error) {
	if !strings.Contains(s, "${input.") {
		return s, nil
	}
	var resolveErr error
	out := inputRefRe.ReplaceAllStringFunc(s, func(token string) string {
		if resolveErr != nil {
			return token // skip remaining substitutions once we've errored
		}
		m := inputRefRe.FindStringSubmatch(token)
		kind, field := m[1], m[2]
		val, err := resolveToken(paramName, token, kind, field, ctx)
		if err != nil {
			resolveErr = err
			return token
		}
		return val
	})
	if resolveErr != nil {
		return "", resolveErr
	}
	return out, nil
}

// resolveToken returns the substitution string for a single matched
// token. kind is "output" or "params"; field is "" for the bare
// ${input.output} form and a Go-identifier for the dotted forms. The
// error path always returns *ErrInputUnavailable with Ref set so the
// caller's wrapping log message includes the offending token.
func resolveToken(paramName, token, kind, field string, ctx InputContext) (string, error) {
	// emptyStringWhy is the canonical Why suffix when a token resolves
	// to "". Empty values are treated as "not provided" uniformly across
	// all three forms — see ErrInputUnavailable's doc comment for the
	// rationale. Surfacing the same phrasing on every form makes the
	// loud-failure contract obvious in logs.
	const emptyStringWhy = "resolves to empty string (empty values are treated as \"not provided\"; pass a non-empty value or omit the reference)"

	switch kind {
	case "output":
		if field == "" {
			// ${input.output} — upstream must be a non-empty string.
			s, ok := ctx.Output.(string)
			if !ok {
				return "", &ErrInputUnavailable{Param: paramName, Ref: token, Why: "upstream returned no string value"}
			}
			if s == "" {
				return "", &ErrInputUnavailable{Param: paramName, Ref: token, Why: emptyStringWhy}
			}
			return s, nil
		}
		// ${input.output.<field>} — upstream must be a map[string]any
		// (the shape JSON decoding produces) with a non-empty string-
		// valued entry at <field>. Maps reached via direct Go callers
		// (e.g. runtime-test mocks) may use map[string]string; accept
		// both.
		switch m := ctx.Output.(type) {
		case map[string]any:
			raw, ok := m[field]
			if !ok {
				return "", &ErrInputUnavailable{Param: paramName, Ref: token, Why: fmt.Sprintf("upstream object has no field %q", field)}
			}
			s, ok := raw.(string)
			if !ok {
				return "", &ErrInputUnavailable{Param: paramName, Ref: token, Why: fmt.Sprintf("upstream field %q is not a string", field)}
			}
			if s == "" {
				return "", &ErrInputUnavailable{Param: paramName, Ref: token, Why: emptyStringWhy}
			}
			return s, nil
		case map[string]string:
			s, ok := m[field]
			if !ok {
				return "", &ErrInputUnavailable{Param: paramName, Ref: token, Why: fmt.Sprintf("upstream object has no field %q", field)}
			}
			if s == "" {
				return "", &ErrInputUnavailable{Param: paramName, Ref: token, Why: emptyStringWhy}
			}
			return s, nil
		default:
			return "", &ErrInputUnavailable{Param: paramName, Ref: token, Why: "upstream return value is not an object"}
		}
	case "params":
		if field == "" {
			// `${input.params}` (no field) is rejected at registration
			// via ValidateInputRefs. If we reach here it's a bypass —
			// fail loudly rather than silently substituting "".
			return "", &ErrInputUnavailable{Param: paramName, Ref: token, Why: "${input.params} requires a named field"}
		}
		if ctx.Params == nil {
			return "", &ErrInputUnavailable{Param: paramName, Ref: token, Why: "upstream params not available"}
		}
		v, ok := ctx.Params[field]
		if !ok {
			return "", &ErrInputUnavailable{Param: paramName, Ref: token, Why: fmt.Sprintf("upstream params has no entry %q", field)}
		}
		if v == "" {
			return "", &ErrInputUnavailable{Param: paramName, Ref: token, Why: emptyStringWhy}
		}
		return v, nil
	default:
		// Unreachable: inputRefRe restricts kind to (output|params).
		return "", &ErrInputUnavailable{Param: paramName, Ref: token, Why: "unknown reference kind"}
	}
}

// ResolveInputOutputMap returns a NEW params map with every string
// value rewritten to substitute recognised `${input.…}` tokens against
// ctx. Non-string values and strings without `${input.…}` pass through
// unchanged. The input map is NOT mutated.
//
// Returns *ErrInputUnavailable on the first param whose tokens cannot
// be resolved (Go map iteration order is undefined; the loud failure
// is what matters, not the specific Param name).
func ResolveInputOutputMap(params map[string]any, ctx InputContext) (map[string]any, error) {
	if params == nil {
		return nil, nil
	}
	out := make(map[string]any, len(params))
	for k, v := range params {
		s, ok := v.(string)
		if !ok {
			out[k] = v
			continue
		}
		resolved, err := resolveString(k, s, ctx)
		if err != nil {
			return nil, err
		}
		out[k] = resolved
	}
	return out, nil
}

// ResolveInputOutputList returns a NEW ParamOverrides slice with every
// Default rewritten the same way as ResolveInputOutputMap. The input
// slice is NOT mutated; each ParamOverride is copied.
func ResolveInputOutputList(params ParamOverrides, ctx InputContext) (ParamOverrides, error) {
	if params == nil {
		return nil, nil
	}
	out := make(ParamOverrides, len(params))
	for i, p := range params {
		out[i] = p // copy by value — ParamOverride is a small struct
		resolved, err := resolveString(p.Name, p.Default, ctx)
		if err != nil {
			return nil, err
		}
		out[i].Default = resolved
	}
	return out, nil
}
