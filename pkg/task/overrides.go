package task

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Defaults are applied to all entries in a TaskSet before per-entry overrides.
// They form level 2 in the three-level precedence stack.
//
// Defined in pkg/task (rather than pkg/taskset) so the same data type can be
// referenced from per-edge override sites in this package (BeforeEntry,
// ChainTrigger.Overrides) without creating an import cycle. pkg/taskset
// re-exports it via a type alias for source compat.
type Defaults struct {
	Timeout time.Duration `yaml:"timeout,omitempty"`
	Retry   *RetryConfig  `yaml:"retry,omitempty"`
	// Env accepts full EnvEntry mappings or bare "KEY" / "KEY=value" strings.
	Env []EnvEntry `yaml:"env,omitempty"`
	// Trigger sets a fallback trigger for any entry that has none.
	Trigger *TriggerPatch `yaml:"trigger,omitempty"`
	Notify  *NotifyConfig `yaml:"notify,omitempty"`
}

// RetryConfig defines automatic retry behaviour for task runs.
type RetryConfig struct {
	Attempts int           `yaml:"attempts"`
	Backoff  time.Duration `yaml:"backoff,omitempty"`
}

// TriggerPatch patches individual sub-fields of a TriggerConfig.
// Pointer fields are nil when not being patched, allowing sub-field
// merges without clearing unrelated trigger types.
type TriggerPatch struct {
	Cron    *string       `yaml:"cron,omitempty"`
	Webhook *string       `yaml:"webhook,omitempty"`
	Auth    *bool         `yaml:"auth,omitempty"`
	Manual  *bool         `yaml:"manual,omitempty"`
	Chain   *ChainTrigger `yaml:"chain,omitempty"`
	Daemon  *bool         `yaml:"daemon,omitempty"`
	Restart *string       `yaml:"restart,omitempty"`
}

// ParamOverride patches the default (and optionally required) of a named param.
// It decodes from either a mapping form  {name: x, default: y}  or a scalar
// "key: value" pair inside a YAML mapping — see ParamOverrides below.
type ParamOverride struct {
	Name     string `yaml:"name"`
	Default  string `yaml:"default"`
	Required *bool  `yaml:"required,omitempty"`
}

// ParamOverrides is a list of ParamOverride values that can be written in two
// equivalent YAML forms:
//
//	# concise map (name → default):
//	params:
//	  provider: google
//	  scope: "user,repo"
//
//	# explicit list (required: supported):
//	params:
//	  - { name: scope, default: "user,repo", required: true }
type ParamOverrides []ParamOverride

func (p *ParamOverrides) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.MappingNode:
		// map form: each key/value pair → ParamOverride{Name: key, Default: value}
		if len(value.Content)%2 != 0 {
			return fmt.Errorf("params mapping has odd number of nodes")
		}
		*p = make(ParamOverrides, 0, len(value.Content)/2)
		for i := 0; i < len(value.Content); i += 2 {
			*p = append(*p, ParamOverride{
				Name:    value.Content[i].Value,
				Default: value.Content[i+1].Value,
			})
		}
		return nil
	case yaml.SequenceNode:
		// list form: decode as []ParamOverride normally
		type plain []ParamOverride
		var list plain
		if err := value.Decode(&list); err != nil {
			return err
		}
		*p = ParamOverrides(list)
		return nil
	default:
		return fmt.Errorf("params must be a mapping or sequence, got %v", value.Tag)
	}
}

// Overrides is a patch applied to a resolved task or to a nested TaskSet entry.
// Fields are applied in the three-level override cascade; later layers win.
//
// Defined in pkg/task (rather than pkg/taskset) so the same data type can be
// referenced from per-edge override sites — BeforeEntry and
// ChainTrigger.Overrides — without creating an import cycle. The merge
// implementation continues to live in pkg/taskset (exported as
// taskset.ApplyOverrides) so per-edge dispatch sites can reuse the exact
// same semantics as the global `dicode tasks override <id>` path.
type Overrides struct {
	Enabled     *bool          `yaml:"enabled,omitempty"`
	Name        string         `yaml:"name,omitempty"`        // replaces spec.Name
	Description string         `yaml:"description,omitempty"` // replaces spec.Description
	Trigger     *TriggerPatch  `yaml:"trigger,omitempty"`
	Params      ParamOverrides `yaml:"params,omitempty"`
	// Env accepts full EnvEntry mappings (name/secret/from/value/optional) or bare "KEY" / "KEY=value" strings.
	Env []EnvEntry `yaml:"env,omitempty"`
	Net []string   `yaml:"net,omitempty"` // replaces permissions.net
	// Fs replaces permissions.fs entirely (matches Net's full-replace pattern).
	Fs      []FSEntry          `yaml:"fs,omitempty"`
	Timeout time.Duration      `yaml:"timeout,omitempty"`
	Retry   *RetryConfig       `yaml:"retry,omitempty"`
	Runtime string             `yaml:"runtime,omitempty"`
	Notify  *NotifyConfig      `yaml:"notify,omitempty"`
	Dicode  *DicodePermissions `yaml:"dicode,omitempty"` // replaces permissions.dicode

	// For task_set entries only — Deprecated: Defaults cross-boundary cascade is no longer applied.
	// Use per-entry overrides.entries[key] to patch nested tasks explicitly.
	Defaults *Defaults `yaml:"defaults,omitempty"`
	// For task_set entries only — Entries patches specific tasks within the nested set.
	Entries map[string]*Overrides `yaml:"entries,omitempty"`
}
