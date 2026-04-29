package config

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
