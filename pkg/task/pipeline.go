package task

import (
	"fmt"
	"strings"
	"time"
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
	Manual        bool          `yaml:"manual,omitempty"`
	Cron          string        `yaml:"cron,omitempty"`
	Webhook       string        `yaml:"webhook,omitempty"`
	WebhookSecret string        `yaml:"webhook_secret,omitempty"`
	WebhookAuth   bool          `yaml:"auth,omitempty"`
	Chain         *ChainTrigger `yaml:"chain,omitempty"`
}

// Stage is one entry in a PipelineTask.Stages list. Structurally identical to
// BeforeEntry, but stage overrides additionally permit a Trigger patch.
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

// pipelineParallelFollowupIssue is referenced in the subtype error so operators
// have a pointer to the planned parallel-mode work. Filled in once filed.
const pipelineParallelFollowupIssue = "the parallel-pipeline support follow-up (planned; not yet filed)"

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
	if p.Subtype != "sequential" {
		return fmt.Errorf("pipeline: subtype %q not implemented in v1 (only \"sequential\"); see %s",
			p.Subtype, pipelineParallelFollowupIssue)
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
				if i == 0 && strings.Contains(pr.Default, "${input.") {
					return fmt.Errorf("pipeline.stages[0].overrides.params.%s: ${input.…} references are not available on the first stage", pr.Name)
				}
				if inputParamsRefRe.MatchString(pr.Default) {
					return fmt.Errorf("pipeline.stages[%d].overrides.params.%s: ${input.params.…} is not available in sequential pipelines", i, pr.Name)
				}
			}
		}
	}
	return nil
}
