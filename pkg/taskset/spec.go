// Package taskset implements the TaskSet architecture: a hierarchical composition
// model for task sources with namespace-scoped IDs, override cascades, and repo
// deduplication.
package taskset

import (
	"fmt"
	"time"

	"github.com/dicode/dicode/pkg/task"
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
	// Watch enables fsnotify on local refs. Nil means "unset — apply default true".
	// Explicit `watch: false` opts out of live reload for this entry.
	Watch *bool `yaml:"watch,omitempty"`
}

// RefAuth holds optional credentials for a git ref.
type RefAuth struct {
	TokenEnv string `yaml:"token_env,omitempty" json:"-"`
	SSHKey   string `yaml:"ssh_key,omitempty" json:"-"`
}

// IsGit reports whether this is a git ref (URL is non-empty).
func (r *Ref) IsGit() bool { return r.URL != "" }

// SourceID computes the canonical Source.ID() for a named entry pointing at
// ref. It is name-qualified so two different dicode.yaml entries that happen
// to reference the identical git URL or local path (e.g. a dynamically-added
// source pointed at a path an existing source already watches, issue #621)
// never collide on Source.ID().
//
// The encoding is length-prefixed — "<len(name)>:<name>:<url-or-path>" —
// rather than a plain "<name>:<url-or-path>" concatenation. A plain
// concatenation is only collision-free if name can never itself contain a
// ':'; that precondition is enforced for API-added sources by
// validateSourceName, but NOT for source names read from dicode.yaml's
// spec.entries keys or nested taskset.yaml entries (pkg/config and
// pkg/taskset's loaders validate the ref, not the entry-name charset), so a
// name like "a:b" paired with path "c" would otherwise produce the same
// string as name "a" paired with path "b:c". Length-prefixing pins down
// exactly where the name ends regardless of what characters name or target
// contain, so two different (name, target) pairs can never collide — the
// digit run before the first ':' can never be misread as part of a shorter
// name, since name's own length is encoded before any of its bytes appear.
//
// Every call site that constructs a taskset.Source (pkg/webui's
// apiAddSource/apiRemoveSource and pkg/daemon's buildTaskSetSourceFromEntry)
// must use this helper so the ID stays consistent between add and remove.
//
// Before this helper existed, ID was just ref.URL (or ref.Path when URL was
// empty) with no name component. pkg/registry/reconciler.go's Reconciler
// keys its cancel-function bookkeeping (rc.cancels) by Source.ID() alone,
// so a collision there silently overwrote one source's cancel func with the
// other's — the second AddSource for a colliding ref would clobber the
// first's teardown entry, and a later RemoveSource(id) for either name could
// cancel the wrong source's context.
func SourceID(name string, ref *Ref) string {
	target := ref.URL
	if target == "" {
		target = ref.Path
	}
	return fmt.Sprintf("%d:%s:%s", len(name), name, target)
}

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

// Override-related types — Defaults, RetryConfig, TriggerPatch,
// ParamOverride, ParamOverrides, Overrides — live in pkg/task so the same
// data type can be referenced from per-edge override sites
// (ChainTrigger.Overrides, PipelineTask Stage overrides) without creating
// an import cycle. They are re-exported here as type aliases so existing
// `taskset.Overrides{}` usage continues to compile unchanged.
type (
	Defaults       = task.Defaults
	RetryConfig    = task.RetryConfig
	TriggerPatch   = task.TriggerPatch
	ParamOverride  = task.ParamOverride
	ParamOverrides = task.ParamOverrides
	Overrides      = task.Overrides
)

// Entry is one named item in spec.entries.
// Exactly one of Ref or Inline must be set.
type Entry struct {
	Ref       *Ref       `yaml:"ref,omitempty"`
	Inline    *task.Spec `yaml:"inline,omitempty"`
	Overrides *Overrides `yaml:"overrides,omitempty"`
	// Enabled is a top-level shortcut for `overrides.enabled`. Useful for
	// one-liner toggling (`enabled: false`) without nesting under overrides.
	// Conflicts with `overrides.enabled` if both are set — the loader rejects
	// that ambiguity rather than picking a winner. Default (omitted) is true.
	Enabled *bool `yaml:"enabled,omitempty"`
	// Tags are UI grouping labels for this entry. Used by the API and web UI to
	// filter or group tasks by source membership.
	Tags []string `yaml:"tags,omitempty"`
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
	// Deprecated: kind:Config spec.defaults no longer affects the override stack; use dicode.yaml defaults: instead.
	Defaults *Defaults `yaml:"defaults,omitempty"`
}

// RuntimePinConfig pins a managed runtime version for all tasks in this source.
type RuntimePinConfig struct {
	Version string `yaml:"version"`
}

// ResolvedTask is a fully resolved task of any kind: for kind: Task, the base
// spec with all override layers applied; for kind: PipelineTask, the validated
// pipeline. It also carries a namespaced ID and the local task directory path.
type ResolvedTask struct {
	Kinded  task.Kinded // resolved task of any kind (overrides already applied)
	ID      string      // namespaced, e.g. "infra/backend/deploy"
	TaskDir string      // absolute local path to the task directory
}
