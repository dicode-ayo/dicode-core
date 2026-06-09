package task

import (
	"os"
	"path/filepath"
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
		{"missing subtype", func(p *PipelineTask) { p.Subtype = "" }, "subtype is required"},
		{"bad subtype", func(p *PipelineTask) { p.Subtype = "parallel" }, "is not supported"},
		{"multi trigger", func(p *PipelineTask) {
			p.Trigger = PipelineTrigger{Manual: true, Cron: "0 * * * *"}
		}, "at most one trigger"},
		{"no stages", func(p *PipelineTask) { p.Stages = nil }, "at least one stage"},
		{"empty stage task", func(p *PipelineTask) { p.Stages[0].Task = "" }, "empty task"},
		{"self ref", func(p *PipelineTask) { p.Stages[0].Task = "p" }, "cannot reference itself"},
		{"dup stage", func(p *PipelineTask) { p.Stages[1].Task = "buildin/template" }, "duplicate"},
		{"input.params on stage1+", func(p *PipelineTask) {
			p.Stages[1].Overrides.Params[0].Default = "${input.params.foo}"
		}, "${input.params"},
		{"stage override rejects enabled", func(p *PipelineTask) {
			en := true
			p.Stages[1].Overrides.Enabled = &en
		}, "not supported at a pipeline stage"},
		{"stage override rejects name", func(p *PipelineTask) {
			p.Stages[1].Overrides.Name = "nope"
		}, "not supported at a pipeline stage"},
		{"stage override rejects description", func(p *PipelineTask) {
			p.Stages[1].Overrides.Description = "nope"
		}, "not supported at a pipeline stage"},
		{"stage override rejects entries", func(p *PipelineTask) {
			p.Stages[1].Overrides.Entries = map[string]*Overrides{"x": {}}
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

// TestPipelineStage0AcceptsInputRefs verifies that stage 0 is allowed to
// reference the trigger payload via ${input.output}, ${input.output.field},
// and ${input.params.X}.
func TestPipelineStage0AcceptsInputRefs(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"output bare", "${input.output}"},
		{"output field", "${input.output.payload}"},
		{"params field", "${input.params.key}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &PipelineTask{
				APIVersion: "dicode/v1", Kind: KindPipelineTask, Name: "P",
				Subtype: "sequential", ID: "p",
				Stages: []Stage{
					{Task: "buildin/template", Overrides: &Overrides{
						Params: ParamOverrides{{Name: "x", Default: tc.value}},
					}},
				},
			}
			if err := p.Validate(); err != nil {
				t.Fatalf("stage 0 ref %q should be accepted, got error: %v", tc.value, err)
			}
		})
	}
}

// TestPipelineStage1PlusRejectsInputParams verifies that ${input.params.X} is
// still rejected on stages ≥1 (no upstream params are threaded between stages).
func TestPipelineStage1PlusRejectsInputParams(t *testing.T) {
	p := &PipelineTask{
		APIVersion: "dicode/v1", Kind: KindPipelineTask, Name: "P",
		Subtype: "sequential", ID: "p",
		Stages: []Stage{
			{Task: "buildin/template"},
			{Task: "buildin/write-local", Overrides: &Overrides{
				Params: ParamOverrides{{Name: "content", Default: "${input.params.foo}"}},
			}},
		},
	}
	err := p.Validate()
	if err == nil {
		t.Fatal("expected error for ${input.params.X} on stage 1, got nil")
	}
	if !strings.Contains(err.Error(), "${input.params") {
		t.Fatalf("error %q does not mention ${input.params", err.Error())
	}
}

// TestPipelineStage1PlusAcceptsInputOutput verifies that ${input.output} and
// ${input.output.field} remain valid on stages ≥1 (they receive the previous
// stage's return value).
func TestPipelineStage1PlusAcceptsInputOutput(t *testing.T) {
	p := &PipelineTask{
		APIVersion: "dicode/v1", Kind: KindPipelineTask, Name: "P",
		Subtype: "sequential", ID: "p",
		Stages: []Stage{
			{Task: "buildin/template"},
			{Task: "buildin/write-local", Overrides: &Overrides{
				Params: ParamOverrides{{Name: "content", Default: "${input.output}"}},
			}},
		},
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("stage 1 ${input.output} should be accepted: %v", err)
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

func TestLoadPipelineDir(t *testing.T) {
	dir := t.TempDir()
	yamlSrc := `apiVersion: dicode/v1
kind: PipelineTask
name: Demo Pipeline
subtype: sequential
trigger:
  manual: true
stages:
  - task: buildin/template
    overrides:
      params:
        template: "hello"
  - task: buildin/write-local
    overrides:
      params:
        content: "${input.output}"
        path: "${DATADIR}/out.txt"
`
	if err := os.WriteFile(filepath.Join(dir, "task.yaml"), []byte(yamlSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := LoadPipelineDir(dir, map[string]string{VarDataDir: "/data"})
	if err != nil {
		t.Fatalf("LoadPipelineDir: %v", err)
	}
	if p.Name != "Demo Pipeline" || len(p.Stages) != 2 {
		t.Fatalf("parsed wrong: %+v", p)
	}
	if p.ID != filepath.Base(dir) {
		t.Fatalf("ID = %q, want %q", p.ID, filepath.Base(dir))
	}
	if !p.Enabled {
		t.Fatal("Enabled should default true")
	}
	// ${DATADIR} expansion applies to stage override params (lookup by name,
	// not position, so a fixture reorder can't silently break this).
	var pathDefault string
	for _, pr := range p.Stages[1].Overrides.Params {
		if pr.Name == "path" {
			pathDefault = pr.Default
		}
	}
	if pathDefault != "/data/out.txt" {
		t.Fatalf("DATADIR not expanded: %q", pathDefault)
	}
}
