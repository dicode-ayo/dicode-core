package task

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// OnFailureChainSpec configures a chained-task fire-on-failure target.
// Accepts either a bare string (task ID) or a structured form with params.
//
//	on_failure_chain: auto-fix
//	# OR
//	on_failure_chain:
//	  task: auto-fix
//	  params:
//	    mode: review
//
// The bare-string form is equivalent to {task: <string>, params: nil}.
type OnFailureChainSpec struct {
	Task   string         `yaml:"task"                     json:"task"`
	Params map[string]any `yaml:"params,omitempty"         json:"params,omitempty"`

	// MaxDepth caps the number of chained on_failure_chain hops. When the
	// incoming run's _chain_depth >= MaxDepth, the chain is suppressed with a
	// structured WARN. Default 2 (see EffectiveMaxDepth).
	MaxDepth int `yaml:"max_depth,omitempty" json:"max_depth,omitempty"`

	// Cooldown is the minimum wall-clock interval between two on_failure_chain
	// fires for the same failing task. Default 10m. Zero = unlimited (no cooldown).
	// Wired by Task 4.
	Cooldown time.Duration `yaml:"cooldown,omitempty" json:"cooldown,omitempty"`

	// MaxConcurrent caps the number of on_failure_chain runs in flight per
	// failing task. Default 1. Wired by Task 5.
	MaxConcurrent int `yaml:"max_concurrent,omitempty" json:"max_concurrent,omitempty"`

	// MaxConcurrentGlobal caps total on_failure_chain runs in flight across all
	// tasks. Applies only at the defaults.on_failure_chain site; per-task values
	// are ignored (enforced at config load in a future work item). Default 3.
	// Wired by Task 6.
	MaxConcurrentGlobal int `yaml:"max_concurrent_global,omitempty" json:"max_concurrent_global,omitempty"`

	// Storm configures the per-source-namespace circuit breaker. Applies only at
	// the defaults site. Zero rate = disabled. Wired by Task 7.
	Storm StormSpec `yaml:"storm,omitempty" json:"storm,omitempty"`
}

// StormSpec is the failure-storm circuit breaker configuration.
type StormSpec struct {
	// Rate is the number of chain fires within Window that trip the breaker.
	Rate int `yaml:"rate,omitempty" json:"rate,omitempty"`
	// Window is the observation period for the rate counter.
	Window time.Duration `yaml:"window,omitempty" json:"window,omitempty"`
	// Suppress is the duration for which chain fires are suppressed after the
	// breaker trips.
	Suppress time.Duration `yaml:"suppress,omitempty" json:"suppress,omitempty"`
	// Scope is "source" (default) or "global".
	Scope string `yaml:"scope,omitempty" json:"scope,omitempty"`
}

// EffectiveMaxDepth returns the configured MaxDepth, or 2 if MaxDepth <= 0.
func (s OnFailureChainSpec) EffectiveMaxDepth() int {
	if s.MaxDepth > 0 {
		return s.MaxDepth
	}
	return 2
}

// UnmarshalYAML decodes either a scalar (bare task ID) or a mapping into the
// struct. Other YAML kinds return an error.
func (s *OnFailureChainSpec) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		var bare string
		if err := value.Decode(&bare); err != nil {
			return err
		}
		s.Task = bare
		s.Params = nil
		return nil
	case yaml.MappingNode:
		// Decode into a plain alias to avoid recursion into UnmarshalYAML.
		type plain OnFailureChainSpec
		var p plain
		if err := value.Decode(&p); err != nil {
			return err
		}
		*s = OnFailureChainSpec(p)
		return nil
	default:
		return fmt.Errorf("on_failure_chain must be a string or mapping, got %v", value.Tag)
	}
}

// IsZero reports whether no chain is configured (bare-string empty or
// uninitialized struct).
func (s OnFailureChainSpec) IsZero() bool {
	return s.Task == ""
}

// reservedChainParamKeys lists keys the engine populates on every chained run's
// input map. User-supplied OnFailureChainSpec.Params may not contain these.
var reservedChainParamKeys = map[string]struct{}{
	"taskID":       {},
	"runID":        {},
	"status":       {},
	"output":       {},
	"_chain_depth": {},
}

// Validate enforces reserved-key constraints and strips per-task-only fields
// (MaxConcurrentGlobal, Storm) that must not be set at the individual task
// level. Called from per-task sites (Spec.validate); not called directly for
// the defaults block — use ValidateAtDefaults for that.
//
// Mutates s in place (zeroes any stripped fields) and returns non-fatal
// warnings that callers should surface via their structured logger.
func (s *OnFailureChainSpec) Validate() (warnings []string, err error) {
	for k := range s.Params {
		if _, reserved := reservedChainParamKeys[k]; reserved {
			return nil, fmt.Errorf("on_failure_chain.params: %q is a reserved key (used by the engine)", k)
		}
	}
	// MaxConcurrentGlobal and Storm are operator-policy fields that apply only
	// at the defaults.on_failure_chain level. A per-task value would bypass the
	// global guard via the full-replace merge in FireChain; zero it out and warn.
	if s.MaxConcurrentGlobal != 0 {
		warnings = append(warnings,
			fmt.Sprintf("on_failure_chain.max_concurrent_global is an operator-policy field and is ignored at the per-task level (task had %d); set it in defaults.on_failure_chain instead", s.MaxConcurrentGlobal))
		s.MaxConcurrentGlobal = 0
	}
	if s.Storm.Rate != 0 || s.Storm.Window != 0 || s.Storm.Suppress != 0 || s.Storm.Scope != "" {
		warnings = append(warnings,
			"on_failure_chain.storm is an operator-policy field and is ignored at the per-task level; set it in defaults.on_failure_chain instead")
		s.Storm = StormSpec{}
	}
	return warnings, nil
}

// ValidateAtDefaults runs at the defaults.on_failure_chain site only.
// Adds the rule: mode: autonomous is rejected at the defaults level — must be
// opted into per-task, paired with branch protection on the source's tracked
// branch.
//
// Does NOT strip MaxConcurrentGlobal or Storm — those are valid here.
func (s OnFailureChainSpec) ValidateAtDefaults() error {
	// Use a pointer receiver call on a local copy to avoid mutating the caller's
	// value; at the defaults site stripping is not needed, but we still need the
	// reserved-key check.
	scratch := s
	if _, err := scratch.Validate(); err != nil {
		return err
	}
	if mode, ok := s.Params["mode"].(string); ok && mode == "autonomous" {
		return fmt.Errorf(
			`defaults.on_failure_chain.params.mode: %q is not allowed at the defaults level. `+
				`Opt each task in via task.yaml on_failure_chain.params.mode (and ensure branch protection on the source's tracked branch).`, mode)
	}
	return nil
}
