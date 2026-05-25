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
// referenced from per-edge override sites in this package (Stage.Overrides on
// kind: PipelineTask, ChainTrigger.Overrides) without creating an import
// cycle. pkg/taskset re-exports it via a type alias for source compat.
type Defaults struct {
	Timeout time.Duration `yaml:"timeout,omitempty"`
	Retry   *RetryConfig  `yaml:"retry,omitempty"`
	// Env accepts full EnvEntry mappings or bare "KEY" / "KEY=value" strings.
	Env []EnvEntry `yaml:"env,omitempty"`
	// Trigger sets a fallback trigger for any entry that has none.
	Trigger *TriggerPatch `yaml:"trigger,omitempty"`
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
// referenced from per-edge override sites — Stage.Overrides on
// kind: PipelineTask and ChainTrigger.Overrides — without creating an import
// cycle. The merge
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
	Dicode  *DicodePermissions `yaml:"dicode,omitempty"` // replaces permissions.dicode

	// For task_set entries only — Deprecated: Defaults cross-boundary cascade is no longer applied.
	// Use per-entry overrides.entries[key] to patch nested tasks explicitly.
	Defaults *Defaults `yaml:"defaults,omitempty"`
	// For task_set entries only — Entries patches specific tasks within the nested set.
	Entries map[string]*Overrides `yaml:"entries,omitempty"`
}

// validatePerEdgeOverrides enforces a conservative allowlist on Overrides
// values used at the per-edge chain dispatch site — i.e.
// trigger.chain.overrides. (kind: PipelineTask stages use the sibling
// validatePipelineStageOverrides, which allows a Trigger patch.) Several
// Overrides fields are meaningful only at the taskset / global level
// (Defaults, Entries, Enabled, Retry, Name, Description) or are silently
// ignored by the per-edge dispatch path (Trigger — see PR #303 MED #3).
// Rejecting them at config-load surfaces operator typos and prevents silent
// footguns.
//
// Allowed at a per-edge site: Params (minus reserved keys), Env, Net, Fs,
// Timeout, Dicode, Runtime.
//
// The site string names the offending location (e.g.
// "trigger.chain.overrides") so operators can find it in their task.yaml.
func validatePerEdgeOverrides(site string, o *Overrides) error {
	if o == nil {
		return nil
	}
	// Enabled — would silently no-op the downstream dispatch. Operators expect
	// it to "skip this edge"; instead it does nothing. Reject so the wrong
	// mental model surfaces at load.
	if o.Enabled != nil {
		return fmt.Errorf("%s: field %q is not supported at a per-edge site (set on the task itself, not on the edge)", site, "enabled")
	}
	// Name / Description — replacing the chained/stage task's identity at the
	// per-edge dispatch makes no semantic sense; the run is logged under the
	// task's real ID regardless. Reject to avoid misleading task.yaml.
	if o.Name != "" {
		return fmt.Errorf("%s: field %q is not supported at a per-edge site", site, "name")
	}
	if o.Description != "" {
		return fmt.Errorf("%s: field %q is not supported at a per-edge site", site, "description")
	}
	// Trigger — PR #303 MED #3. The chain dispatch path invokes the
	// downstream directly and ignores any rewired trigger config on the
	// merged spec. Allowing `trigger:` here misleads operators into thinking
	// they're rewiring the downstream's trigger graph for this firing.
	if o.Trigger != nil {
		return fmt.Errorf("%s: field %q is not supported at a per-edge site (per-edge dispatch ignores the downstream's trigger config)", site, "trigger")
	}
	// Retry — would apply per-firing, almost certainly unintended. If
	// per-edge retries become a real feature request, lift this
	// rejection and wire applyLayer to copy it.
	if o.Retry != nil {
		return fmt.Errorf("%s: field %q is not supported at a per-edge site", site, "retry")
	}
	// Defaults / Entries — taskset-level constructs with no per-edge
	// meaning.
	if o.Defaults != nil {
		return fmt.Errorf("%s: field %q is not supported at a per-edge site (taskset-level construct)", site, "defaults")
	}
	if o.Entries != nil {
		return fmt.Errorf("%s: field %q is not supported at a per-edge site (taskset-level construct)", site, "entries")
	}
	// Params: same reserved-key check used on trigger.chain.params /
	// on_failure_chain.params. Reserved engine keys must not appear in
	// user-supplied per-edge params either — otherwise they'd collide with
	// the input map the engine populates at firing time.
	for _, p := range o.Params {
		if _, reserved := reservedChainParamKeys[p.Name]; reserved {
			return fmt.Errorf("%s: params %q is a reserved key (used by the engine)", site, p.Name)
		}
		// Static validation of `${input.…}` references on per-edge
		// override defaults — same gate as trigger.chain.params /
		// on_failure_chain.params. Catches malformed shapes at config
		// load. (PipelineTask stage[0] additionally rejects ANY
		// `${input.…}` ref — see PipelineTask.Validate.)
		if err := ValidateInputRefs(fmt.Sprintf("%s.params.%s", site, p.Name), p.Default); err != nil {
			return err
		}
	}
	return nil
}

// validatePipelineStageOverrides is the per-stage override allowlist for
// kind: PipelineTask. It is identical to validatePerEdgeOverrides EXCEPT it
// permits a Trigger patch — a pipeline may override a stage task's trigger
// (e.g. flip a manual-only task to fireable). Params/Env/Net/Fs/Timeout/
// Dicode/Runtime are allowed; Enabled/Name/Description/Retry/Defaults/Entries
// are rejected.
func validatePipelineStageOverrides(site string, o *Overrides) error {
	if o == nil {
		return nil
	}
	if o.Enabled != nil {
		return fmt.Errorf("%s: field %q is not supported at a pipeline stage", site, "enabled")
	}
	if o.Name != "" {
		return fmt.Errorf("%s: field %q is not supported at a pipeline stage", site, "name")
	}
	if o.Description != "" {
		return fmt.Errorf("%s: field %q is not supported at a pipeline stage", site, "description")
	}
	if o.Retry != nil {
		return fmt.Errorf("%s: field %q is not supported at a pipeline stage", site, "retry")
	}
	if o.Defaults != nil {
		return fmt.Errorf("%s: field %q is not supported at a pipeline stage (taskset-level construct)", site, "defaults")
	}
	if o.Entries != nil {
		return fmt.Errorf("%s: field %q is not supported at a pipeline stage (taskset-level construct)", site, "entries")
	}
	for _, p := range o.Params {
		if _, reserved := reservedChainParamKeys[p.Name]; reserved {
			return fmt.Errorf("%s: params %q is a reserved key (used by the engine)", site, p.Name)
		}
		if err := ValidateInputRefs(fmt.Sprintf("%s.params.%s", site, p.Name), p.Default); err != nil {
			return err
		}
	}
	return nil
}
