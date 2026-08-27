package task

import (
	"fmt"
	"io"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// PipelineTask is the spec for kind: PipelineTask — an orchestration of one or
// more kind: Task stages. subtype: sequential runs stages in order; subtype:
// parallel runs stages concurrently (with optional depends_on for DAG ordering).
// It is a peer of Spec, not a Spec.
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
	Manual           bool            `yaml:"manual,omitempty"`
	Cron             string          `yaml:"cron,omitempty"`
	Webhook          string          `yaml:"webhook,omitempty"`
	WebhookSecret    string          `yaml:"webhook_secret,omitempty"`
	WebhookAuth      WebhookAuthMode `yaml:"auth,omitempty"`
	ReplayProtection *bool           `yaml:"replay_protection,omitempty"`
	RequireTimestamp *bool           `yaml:"require_timestamp,omitempty"`
	Chain            *ChainTrigger   `yaml:"chain,omitempty"`
}

// Stage is one entry in a PipelineTask.Stages list: a task ID plus optional
// per-stage overrides. Stage overrides permit a Trigger patch (a stage can
// override its task's trigger), unlike the chain-edge override allowlist.
type Stage struct {
	Task      string     `yaml:"task"`
	DependsOn []string   `yaml:"depends_on,omitempty" json:"depends_on,omitempty"`
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

// LoadPipelineDir parses a kind: PipelineTask from <dir>/task.yaml (or
// task.yml — see openTaskSpecFile). The caller is responsible for having
// already determined the kind (see LoadKindedDir). Use LoadPipelineDirFile
// instead when the caller already knows the exact manifest filename a prior
// kind-detection pass vetted.
func LoadPipelineDir(dir string, extras map[string]string) (*PipelineTask, error) {
	return loadPipelineDir(dir, "", extras)
}

// LoadPipelineDirFile is LoadPipelineDir for a caller that already knows the
// exact manifest filename to load — see LoadDirWithVarsFile's doc comment
// for why this matters, including why filename must be non-empty and that
// precondition is enforced rather than silently falling back to probing.
func LoadPipelineDirFile(dir, filename string, extras map[string]string) (*PipelineTask, error) {
	if filename == "" {
		return nil, fmt.Errorf("LoadPipelineDirFile: filename must be non-empty for %s (use LoadPipelineDir for probing)", dir)
	}
	return loadPipelineDir(dir, filename, extras)
}

func loadPipelineDir(dir, filename string, extras map[string]string) (*PipelineTask, error) {
	f, specPath, err := openTaskSpecFile(dir, filename)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", specPath, err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", specPath, err)
	}

	// Probe for the removed `notify:` block before decoding (mirrors LoadDirWithVars).
	var probe struct {
		Notify any `yaml:"notify"`
	}
	if err := yaml.Unmarshal(data, &probe); err == nil && probe.Notify != nil {
		return nil, fmt.Errorf("%s: legacy `notify` block detected. "+
			"The per-task notify field was removed (#279). Use `on_failure_chain` "+
			"to fire a notification task on failure — see docs", specPath)
	}

	var p PipelineTask
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse %s: %w", specPath, err)
	}

	// Set ID before Validate: PipelineTask.Validate's self-reference check
	// compares each stage against p.ID, so it must be populated first.
	// (This differs from LoadDirWithVars's ordering.)
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
	// trigger.webhook_secret: same env-fallback policy as kind: Task (see
	// expandSpec) — resolved server-side, never readable from task code.
	p.Trigger.WebhookSecret = expandString(p.Trigger.WebhookSecret, vars, true)
	for i := range p.Stages {
		expandOverrides(p.Stages[i].Overrides, vars)
	}
	// After expansion: downgrade auth: any to session when the secret is empty
	// or an unresolved ${VAR} placeholder, so a committed placeholder is never
	// served as a live HMAC key (mirrors normalizeWebhookAuth for kind: Task).
	normalizeWebhookAuthFields(&p.Trigger.WebhookAuth, &p.Trigger.WebhookSecret,
		&p.Trigger.ReplayProtection, &p.Trigger.RequireTimestamp, &p.Warnings)
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
		return fmt.Errorf("pipeline: subtype is required (use \"sequential\" or \"parallel\")")
	}
	if p.Subtype != "sequential" && p.Subtype != "parallel" {
		return fmt.Errorf("pipeline: subtype %q is not supported (use \"sequential\" or \"parallel\")", p.Subtype)
	}
	if p.Trigger.count() > 1 {
		return fmt.Errorf("pipeline: at most one trigger type may be set")
	}
	if p.Trigger.WebhookAuth == WebhookAuthAny && p.Trigger.WebhookSecret == "" {
		return fmt.Errorf(`pipeline: trigger.auth: "any" requires webhook_secret (the HMAC path has nothing to verify without it)`)
	}
	p.Warnings = append(p.Warnings, webhookSecretGatedFieldWarnings(p.Trigger.WebhookSecret, p.Trigger.ReplayProtection, p.Trigger.RequireTimestamp)...)
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

		// depends_on is only valid for parallel pipelines.
		if len(st.DependsOn) > 0 && p.Subtype != "parallel" {
			return fmt.Errorf("pipeline: stage %d (%s): depends_on is only valid for subtype \"parallel\"", i, st.Task)
		}
		// Validate depends_on references point to existing stage task IDs.
		for _, dep := range st.DependsOn {
			if _, ok := seen[dep]; !ok {
				// The dependency must reference a stage that was listed BEFORE
				// this stage (already in `seen`). This also rejects forward
				// references and unknown task IDs.
				return fmt.Errorf("pipeline: stage %d (%s): depends_on references unknown or forward stage %q", i, st.Task, dep)
			}
		}

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
				// For parallel pipelines, input.params refs are only valid on
				// root stages (no depends_on).
				isRoot := len(st.DependsOn) == 0
				if p.Subtype == "sequential" && i > 0 && inputParamsRefRe.MatchString(pr.Default) {
					return fmt.Errorf("pipeline.stages[%d].overrides.params.%s: ${input.params.…} is not available on stages after stage 0 (no upstream params are threaded between stages)", i, pr.Name)
				}
				if p.Subtype == "parallel" && !isRoot && inputParamsRefRe.MatchString(pr.Default) {
					return fmt.Errorf("pipeline.stages[%d].overrides.params.%s: ${input.params.…} is not available on stages with depends_on (no upstream params are threaded between stages)", i, pr.Name)
				}
			}
		}
	}

	// DAG cycle detection for parallel pipelines: since depends_on only allows
	// references to stages listed earlier (validated above), cycles are
	// structurally impossible. But we still detect them defensively.
	if p.Subtype == "parallel" {
		if cycle := detectStageCycle(p.Stages); cycle != "" {
			return fmt.Errorf("pipeline: cycle detected in depends_on: %s", cycle)
		}
	}

	return nil
}

// detectStageCycle runs a DFS on the depends_on graph and returns a printable
// cycle path or "". Since depends_on only allows backward references (validated
// during stage iteration), this should be unreachable, but is retained for
// defense-in-depth.
func detectStageCycle(stages []Stage) string {
	edges := make(map[string][]string, len(stages))
	for _, st := range stages {
		edges[st.Task] = st.DependsOn
	}

	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(stages))
	var stack []string
	var dfs func(id string) string
	dfs = func(id string) string {
		color[id] = gray
		stack = append(stack, id)
		for _, next := range edges[id] {
			switch color[next] {
			case gray:
				start := 0
				for idx, n := range stack {
					if n == next {
						start = idx
						break
					}
				}
				path := append([]string(nil), stack[start:]...)
				path = append(path, next)
				return fmt.Sprintf("%s", joinArrow(path))
			case white:
				if cp := dfs(next); cp != "" {
					return cp
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
		return ""
	}
	for _, st := range stages {
		if color[st.Task] == white {
			if cp := dfs(st.Task); cp != "" {
				return cp
			}
		}
	}
	return ""
}

func joinArrow(parts []string) string {
	result := parts[0]
	for _, p := range parts[1:] {
		result += " -> " + p
	}
	return result
}
