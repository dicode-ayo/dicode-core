package task

import (
	"fmt"

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
	Task   string         `yaml:"task"`
	Params map[string]any `yaml:"params,omitempty"`
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

// Validate enforces reserved-key constraints. Called from both
// the defaults site (Defaults.OnFailureChain) and per-task sites
// (Task.Spec.OnFailureChain).
func (s OnFailureChainSpec) Validate() error {
	for k := range s.Params {
		if _, reserved := reservedChainParamKeys[k]; reserved {
			return fmt.Errorf("on_failure_chain.params: %q is a reserved key (used by the engine)", k)
		}
	}
	return nil
}

// ValidateAtDefaults runs at the defaults.on_failure_chain site only.
// Adds the rule: mode: autonomous is rejected at the defaults level — must be
// opted into per-task, paired with branch protection on the source's tracked
// branch.
func (s OnFailureChainSpec) ValidateAtDefaults() error {
	if err := s.Validate(); err != nil {
		return err
	}
	if mode, ok := s.Params["mode"].(string); ok && mode == "autonomous" {
		return fmt.Errorf(
			`defaults.on_failure_chain.params.mode: %q is not allowed at the defaults level. `+
				`Opt each task in via task.yaml on_failure_chain.params.mode (and ensure branch protection on the source's tracked branch).`, mode)
	}
	return nil
}
