package task

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// PipelineTask is the spec for kind: PipelineTask — a sequential orchestration
// of one or more kind: Task stages. It is a peer of Spec, not a Spec.
type PipelineTask struct {
	APIVersion  string          `yaml:"apiVersion" json:"apiVersion"`
	Kind        string          `yaml:"kind"       json:"kind"`
	Name        string          `yaml:"name"       json:"name"`
	Description string          `yaml:"description,omitempty" json:"description,omitempty"`
	Subtype     string          `yaml:"subtype"    json:"subtype"` // "sequential" (v1) | "parallel" (v2+)
	Trigger     PipelineTrigger `yaml:"trigger,omitempty" json:"trigger"`
	Stages      []Stage         `yaml:"stages"     json:"stages"`
	Timeout     time.Duration   `yaml:"timeout,omitempty" json:"timeout,omitempty"`

	// Not stored in YAML — set by the loader/reconciler (mirrors Spec).
	TaskDir  string   `yaml:"-" json:"-"`
	ID       string   `yaml:"-" json:"id"`
	Enabled  bool     `yaml:"-" json:"enabled"`
	Warnings []string `yaml:"-" json:"-"`
}

// PipelineTrigger is the subset of trigger shapes valid on a pipeline.
// Notably no Daemon: a pipeline is daemon-shaped iff its terminal stage is a
// kind: Task with trigger.daemon: true.
type PipelineTrigger struct {
	Manual           bool          `yaml:"manual,omitempty"`
	Cron             string        `yaml:"cron,omitempty"`
	Webhook          string        `yaml:"webhook,omitempty"`
	WebhookSecret    string        `yaml:"webhook_secret,omitempty"`
	WebhookAuth      bool          `yaml:"auth,omitempty"`
	ReplayProtection *bool         `yaml:"replay_protection,omitempty"`
	Chain            *ChainTrigger `yaml:"chain,omitempty"`
}

// Stage is one entry in a PipelineTask.Stages list: a task ID plus optional
// per-stage overrides. Stage overrides permit a Trigger patch (a stage can
// override its task's trigger), unlike the chain-edge override allowlist.
type Stage struct {
	Task      string     `yaml:"task"`
	Overrides *Overrides `yaml:"overrides,omitempty"`
}

func (p *PipelineTask) KindOf() string         { return KindPipelineTask }
func (p *PipelineTask) TaskID() string         { return p.ID }
func (p *PipelineTask) SetTaskID(id string)    { p.ID = id }
func (p *PipelineTask) IsEnabled() bool        { return p.Enabled }
func (p *PipelineTask) SetEnabled(b bool)      { p.Enabled = b }
func (p *PipelineTask) LoadWarnings() []string { return p.Warnings }

// Compile-time assertion that PipelineTask satisfies Kinded.
var _ Kinded = (*PipelineTask)(nil)

// count reports how many trigger shapes are set (for the at-most-one check).
func (t PipelineTrigger) count() int {
	n := 0
	if t.Manual {
		n++
	}
	if t.Cron != "" {
		n++
	}
	if t.Webhook != "" {
		n++
	}
	if t.Chain != nil {
		n++
	}
	return n
}

// LoadPipelineDir parses a kind: PipelineTask from <dir>/task.yaml. The caller
// is responsible for having already determined the kind (see LoadKindedDir).
func LoadPipelineDir(dir string, extras map[string]string) (*PipelineTask, error) {
	specPath := filepath.Join(dir, "task.yaml")
	f, err := os.Open(specPath)
	if err != nil {
		return nil, fmt.Errorf("open task.yaml in %s: %w", dir, err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read task.yaml in %s: %w", dir, err)
	}

	// Probe for the removed `notify:` block before decoding (mirrors LoadDirWithVars).
	var probe struct {
		Notify any `yaml:"notify"`
	}
	if err := yaml.Unmarshal(data, &probe); err == nil && probe.Notify != nil {
		return nil, fmt.Errorf("task.yaml in %s: legacy `notify` block detected. "+
			"The per-task notify field was removed (#279). Use `on_failure_chain` "+
			"to fire a notification task on failure — see docs.", dir)
	}

	var p PipelineTask
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse task.yaml in %s: %w", dir, err)
	}

	// Set ID before Validate: PipelineTask.Validate's self-reference check
	// compares each stage against p.ID, so it must be populated first.
	// (This intentionally differs from LoadDirWithVars's ordering.)
	p.TaskDir = dir
	p.ID = filepath.Base(dir)
	p.Enabled = true

	expandPipeline(&p, builtinVars(dir, extras))

	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("invalid pipeline in %s: %w", dir, err)
	}
	return &p, nil
}

// expandPipeline expands ${VAR} template variables in stage override params and
// fs paths, reusing expandOverrides (which mirrors the envFallback policy from
// expandSpec: Params[].Default gets envFallback=false, Fs[].Path gets true).
func expandPipeline(p *PipelineTask, vars map[string]string) {
	for i := range p.Stages {
		expandOverrides(p.Stages[i].Overrides, vars)
	}
}

// Validate runs all load-time (non-registry) checks. Cross-spec checks (stage
// existence, cycle detection, stages-must-be-kind-Task) run in the engine's
// registerPipeline because they need the registry snapshot.
func (p *PipelineTask) Validate() error {
	if p.APIVersion != "dicode/v1" {
		return fmt.Errorf("pipeline: apiVersion must be %q, got %q", "dicode/v1", p.APIVersion)
	}
	if p.Kind != KindPipelineTask {
		return fmt.Errorf("pipeline: kind must be %q, got %q", KindPipelineTask, p.Kind)
	}
	if p.Name == "" {
		return fmt.Errorf("pipeline: name is required")
	}
	if p.Subtype == "" {
		return fmt.Errorf("pipeline: subtype is required (use \"sequential\")")
	}
	if p.Subtype != "sequential" {
		return fmt.Errorf("pipeline: subtype %q is not supported in v1 (only \"sequential\"; parallel pipelines are a planned follow-up)", p.Subtype)
	}
	if p.Trigger.count() > 1 {
		return fmt.Errorf("pipeline: at most one trigger type may be set")
	}
	if len(p.Stages) == 0 {
		return fmt.Errorf("pipeline: at least one stage is required")
	}
	seen := make(map[string]struct{}, len(p.Stages))
	for i, st := range p.Stages {
		if st.Task == "" {
			return fmt.Errorf("pipeline: stage %d has an empty task ID", i)
		}
		if st.Task == p.ID {
			return fmt.Errorf("pipeline: stage %d cannot reference itself (%q)", i, st.Task)
		}
		if _, dup := seen[st.Task]; dup {
			return fmt.Errorf("pipeline: stage %d duplicate task ID %q (v1 has no stage-id disambiguation)", i, st.Task)
		}
		seen[st.Task] = struct{}{}

		if st.Overrides != nil {
			site := fmt.Sprintf("pipeline.stages[%d].overrides (task %q)", i, st.Task)
			if err := validatePipelineStageOverrides(site, st.Overrides); err != nil {
				return err
			}
			for _, pr := range st.Overrides.Params {
				// Stage 0 may reference ${input.*} — the trigger payload seeds it (#350).
				// Stages ≥1 have no upstream params map, so ${input.params.*} is still
				// rejected there; ${input.output} / ${input.output.<field>} remain valid
				// on all stages (stage→stage threading).
				if i > 0 && inputParamsRefRe.MatchString(pr.Default) {
					return fmt.Errorf("pipeline.stages[%d].overrides.params.%s: ${input.params.…} is not available on stages after stage 0 (no upstream params are threaded between stages)", i, pr.Name)
				}
			}
		}
	}
	return nil
}
