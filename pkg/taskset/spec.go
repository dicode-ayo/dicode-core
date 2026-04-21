// Package taskset implements the TaskSet architecture: a hierarchical composition
// model for task sources with namespace-scoped IDs, override cascades, and repo
// deduplication.
package taskset

import (
	"fmt"
	"time"

	"github.com/dicode/dicode/pkg/task"
	"gopkg.in/yaml.v3"
)

// Ref points to a yaml file (kind: Task or kind: TaskSet).
// If URL is non-empty it is a git ref; otherwise Path is an absolute local path.
type Ref struct {
	URL          string        `yaml:"url,omitempty"`
	Path         string        `yaml:"path"`
	Branch       string        `yaml:"branch,omitempty"`
	PollInterval time.Duration `yaml:"poll_interval,omitempty"`
	Auth         RefAuth       `yaml:"auth,omitempty"`
	// DevRef is substituted in place of this ref when dev mode is active.
	DevRef *Ref `yaml:"dev_ref,omitempty"`
}

// RefAuth holds optional credentials for a git ref.
type RefAuth struct {
	TokenEnv string `yaml:"token_env,omitempty"`
	SSHKey   string `yaml:"ssh_key,omitempty"`
}

// IsGit reports whether this is a git ref (URL is non-empty).
func (r *Ref) IsGit() bool { return r.URL != "" }

// effectiveBranch returns the branch, defaulting to "main".
func (r *Ref) effectiveBranch() string {
	if r.Branch != "" {
		return r.Branch
	}
	return "main"
}

// effectivePoll returns the poll interval, defaulting to 30s.
func (r *Ref) effectivePoll() time.Duration {
	if r.PollInterval > 0 {
		return r.PollInterval
	}
	return 30 * time.Second
}

// Defaults are applied to all entries in a TaskSet before per-entry overrides.
// They form level 2 in the three-level precedence stack.
type Defaults struct {
	Timeout time.Duration `yaml:"timeout,omitempty"`
	Retry   *RetryConfig  `yaml:"retry,omitempty"`
	// Env accepts full EnvEntry mappings or bare "KEY" / "KEY=value" strings.
	Env []task.EnvEntry `yaml:"env,omitempty"`
	// Trigger sets a fallback trigger for any entry that has none.
	Trigger *TriggerPatch      `yaml:"trigger,omitempty"`
	Notify  *task.NotifyConfig `yaml:"notify,omitempty"`
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
	Cron    *string            `yaml:"cron,omitempty"`
	Webhook *string            `yaml:"webhook,omitempty"`
	Auth    *bool              `yaml:"auth,omitempty"`
	Manual  *bool              `yaml:"manual,omitempty"`
	Chain   *task.ChainTrigger `yaml:"chain,omitempty"`
	Daemon  *bool              `yaml:"daemon,omitempty"`
	Restart *string            `yaml:"restart,omitempty"`
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
type Overrides struct {
	Enabled     *bool          `yaml:"enabled,omitempty"`
	Name        string         `yaml:"name,omitempty"`        // replaces spec.Name
	Description string         `yaml:"description,omitempty"` // replaces spec.Description
	Trigger     *TriggerPatch  `yaml:"trigger,omitempty"`
	Params      ParamOverrides `yaml:"params,omitempty"`
	// Env accepts full EnvEntry mappings (name/secret/from/value/optional) or bare "KEY" / "KEY=value" strings.
	Env     []task.EnvEntry         `yaml:"env,omitempty"`
	Net     []string                `yaml:"net,omitempty"` // replaces permissions.net
	Timeout time.Duration           `yaml:"timeout,omitempty"`
	Retry   *RetryConfig            `yaml:"retry,omitempty"`
	Runtime string                  `yaml:"runtime,omitempty"`
	Notify  *task.NotifyConfig      `yaml:"notify,omitempty"`
	Dicode  *task.DicodePermissions `yaml:"dicode,omitempty"` // replaces permissions.dicode

	// For task_set entries only — Deprecated: Defaults cross-boundary cascade is no longer applied.
	// Use per-entry overrides.entries[key] to patch nested tasks explicitly.
	Defaults *Defaults `yaml:"defaults,omitempty"`
	// For task_set entries only — Entries patches specific tasks within the nested set.
	Entries map[string]*Overrides `yaml:"entries,omitempty"`
}

// Entry is one named item in spec.entries.
// Exactly one of Ref or Inline must be set.
type Entry struct {
	Ref       *Ref       `yaml:"ref,omitempty"`
	Inline    *task.Spec `yaml:"inline,omitempty"`
	Overrides *Overrides `yaml:"overrides,omitempty"`
}

// TaskSetSpec is parsed from a yaml file with kind: TaskSet.
type TaskSetSpec struct {
	APIVersion string      `yaml:"apiVersion"`
	Kind       string      `yaml:"kind"`
	Metadata   TSMetadata  `yaml:"metadata"`
	Spec       TaskSetBody `yaml:"spec"`
}

// TSMetadata holds the metadata block of a TaskSet or Config file.
type TSMetadata struct {
	Name string `yaml:"name"`
}

// TaskSetBody is the spec block of a TaskSet.
type TaskSetBody struct {
	// Defaults are applied at level 1 in the three-level precedence stack (below per-entry overrides).
	Defaults *Defaults         `yaml:"defaults,omitempty"`
	Entries  map[string]*Entry `yaml:"entries"`
}

// ConfigSpec is parsed from a yaml file with kind: Config.
// It scopes runtime pins, notification routing, and task defaults to one source.
type ConfigSpec struct {
	APIVersion string     `yaml:"apiVersion"`
	Kind       string     `yaml:"kind"`
	Metadata   TSMetadata `yaml:"metadata"`
	Spec       ConfigBody `yaml:"spec"`
}

// ConfigBody is the spec block of a Config file.
type ConfigBody struct {
	Runtimes map[string]RuntimePinConfig `yaml:"runtimes,omitempty"`
	// Defaults previously sat at precedence level 2 in the old six-level stack.
	// Deprecated: kind:Config spec.defaults no longer affects the override stack; use dicode.yaml defaults: instead.
	Defaults *Defaults `yaml:"defaults,omitempty"`
}

// RuntimePinConfig pins a managed runtime version for all tasks in this source.
type RuntimePinConfig struct {
	Version string `yaml:"version"`
}

// ResolvedTask is a fully resolved task: base spec with all override layers applied,
// a namespaced ID, and the local path to the task directory.
type ResolvedTask struct {
	Spec    *task.Spec
	ID      string // namespaced, e.g. "infra/backend/deploy"
	TaskDir string // absolute local path to the task directory
}
