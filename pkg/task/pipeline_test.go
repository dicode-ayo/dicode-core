package task

import (
	"strings"
	"testing"
)

func TestPipelineTaskImplementsKinded(t *testing.T) {
	var k Kinded = &PipelineTask{ID: "p", Name: "P", Subtype: "sequential", Enabled: true,
		Stages: []Stage{{Task: "buildin/template"}}}
	if k.KindOf() != KindPipelineTask {
		t.Fatalf("KindOf = %q, want %q", k.KindOf(), KindPipelineTask)
	}
	if k.TaskID() != "p" {
		t.Fatalf("TaskID = %q, want p", k.TaskID())
	}
	k.SetTaskID("q")
	k.SetEnabled(false)
	if k.TaskID() != "q" || k.IsEnabled() {
		t.Fatal("setters failed")
	}
}

func validPipeline() *PipelineTask {
	return &PipelineTask{
		APIVersion: "dicode/v1", Kind: KindPipelineTask, Name: "P",
		Subtype: "sequential", ID: "p",
		Stages: []Stage{
			{Task: "buildin/template"},
			{Task: "buildin/write-local", Overrides: &Overrides{
				Params: ParamOverrides{{Name: "content", Default: "${input.output}"}}}},
		},
	}
}

func TestPipelineValidate(t *testing.T) {
	if err := validPipeline().Validate(); err != nil {
		t.Fatalf("valid pipeline rejected: %v", err)
	}
	cases := []struct {
		name string
		mut  func(p *PipelineTask)
		want string
	}{
		{"bad apiVersion", func(p *PipelineTask) { p.APIVersion = "v2" }, "apiVersion"},
		{"bad kind", func(p *PipelineTask) { p.Kind = "Task" }, "kind"},
		{"no name", func(p *PipelineTask) { p.Name = "" }, "name"},
		{"bad subtype", func(p *PipelineTask) { p.Subtype = "parallel" }, "not implemented in v1"},
		{"multi trigger", func(p *PipelineTask) {
			p.Trigger = PipelineTrigger{Manual: true, Cron: "0 * * * *"}
		}, "at most one trigger"},
		{"no stages", func(p *PipelineTask) { p.Stages = nil }, "at least one stage"},
		{"empty stage task", func(p *PipelineTask) { p.Stages[0].Task = "" }, "empty task"},
		{"self ref", func(p *PipelineTask) { p.Stages[0].Task = "p" }, "cannot reference itself"},
		{"dup stage", func(p *PipelineTask) { p.Stages[1].Task = "buildin/template" }, "duplicate"},
		{"input on stage0", func(p *PipelineTask) {
			p.Stages[0].Overrides = &Overrides{Params: ParamOverrides{{Name: "x", Default: "${input.output}"}}}
		}, "first stage"},
		{"input.params anywhere", func(p *PipelineTask) {
			p.Stages[1].Overrides.Params[0].Default = "${input.params.foo}"
		}, "${input.params"},
		{"stage override rejects enabled", func(p *PipelineTask) {
			en := true
			p.Stages[1].Overrides.Enabled = &en
		}, "not supported at a pipeline stage"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validPipeline()
			tc.mut(p)
			err := p.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestPipelineStageTriggerOverrideAllowed(t *testing.T) {
	p := validPipeline()
	disable := false
	p.Stages[0].Overrides = &Overrides{Trigger: &TriggerPatch{Manual: &disable}}
	if err := p.Validate(); err != nil {
		t.Fatalf("stage trigger override should be allowed: %v", err)
	}
}
