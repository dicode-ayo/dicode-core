package trigger

import (
	"context"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
)

// TestPipelineRunnerSequential fires a two-stage sequential pipeline and asserts
// the parent run is kind=pipeline + success, both stages ran as pipeline-stage
// children, and ${input.output} threaded stage-a's return into stage-b's param.
func TestPipelineRunnerSequential(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	env := newTestEnv(t)
	dir := t.TempDir()

	// stage-a returns a string; stage-b echoes its `content` param, which the
	// pipeline wires to ${input.output} (stage-a's return).
	stageA := writeTask(t, dir, "stage-a",
		`export default async function main() { return "hello" }`,
		task.TriggerConfig{Manual: true})
	stageB := writeTask(t, dir, "stage-b",
		`export default async function main({ params }) { return await params.get("content") }`,
		task.TriggerConfig{Manual: true})
	for _, s := range []*task.Spec{stageA, stageB} {
		if err := env.reg.Register(s); err != nil {
			t.Fatalf("reg.Register %s: %v", s.ID, err)
		}
		if err := env.engine.Register(s); err != nil {
			t.Fatalf("eng.Register %s: %v", s.ID, err)
		}
	}

	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "p", Name: "P", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Manual: true},
		Stages: []task.Stage{
			{Task: "stage-a"},
			{Task: "stage-b", Overrides: &task.Overrides{
				Params: task.ParamOverrides{{Name: "content", Default: "${input.output}"}}}},
		},
	}
	if err := env.reg.Register(pipe); err != nil {
		t.Fatalf("reg.Register pipe: %v", err)
	}
	if err := env.engine.Register(pipe); err != nil {
		t.Fatalf("eng.Register pipe: %v", err)
	}

	parentRunID, err := env.engine.FireManual(context.Background(), "p", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}

	parent := waitForTerminal(t, env.engine, parentRunID, 30*time.Second)
	if parent.Kind != registry.RunKindPipeline {
		t.Fatalf("parent kind = %q, want %q", parent.Kind, registry.RunKindPipeline)
	}
	if parent.Status != registry.StatusSuccess {
		t.Fatalf("parent status = %q (reason=%q), want success", parent.Status, parent.FailureReason)
	}

	kids, err := env.reg.ListChildren(context.Background(), parentRunID, 10)
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	if len(kids) != 2 {
		t.Fatalf("want 2 stage children, got %d: %+v", len(kids), kids)
	}
	for _, c := range kids {
		if c.TriggerSource != registry.TriggerPipelineStage {
			t.Errorf("child %s source = %q, want %q", c.TaskID, c.TriggerSource, registry.TriggerPipelineStage)
		}
		if c.ParentRunID != parentRunID {
			t.Errorf("child %s parent = %q, want %q", c.TaskID, c.ParentRunID, parentRunID)
		}
	}

	// ${input.output} threading: the pipeline's own return value is stage-b's
	// return, which echoes stage-a's "hello".
	res, err := env.engine.WaitRun(context.Background(), parentRunID)
	if err != nil {
		t.Fatalf("WaitRun parent: %v", err)
	}
	if res.ReturnValue != "hello" {
		t.Errorf("pipeline return = %v, want \"hello\"", res.ReturnValue)
	}
}
