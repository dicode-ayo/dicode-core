package trigger

// End-to-end test for the kind: PipelineTask runner (PR3). Composes the REAL
// buildin/template and buildin/write-local tasks (loaded from disk) into a
// 2-stage sequential pipeline fired manually through real Deno:
//
//   1. buildin/template renders a literal config body (no ${VAR} placeholders →
//      no env wiring). Its on-disk run_result.enabled:false means the rendered
//      string flows in-memory via ${input.output} rather than the persisted
//      runs.return_value column — exercising the WaitRun cache fallback.
//   2. buildin/write-local persists the rendered string to a caller-declared
//      path. Its permissions.fs ships empty, so the stage override declares an
//      fs:rw scope covering the target path.
//
// Assertions:
//   - The rendered file lands on disk (proves template ran with its override
//     AND write-local received the rendered string via ${input.output}).
//   - The pipeline parent run is success.
//   - Both stages have their own runs tagged TriggerPipelineStage, linked to
//     the parent (data-model linkage for the WebUI).
//
// Gated on real Deno (newTestEnv skips when absent).

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
)

func TestE2E_PipelineTask_RealDeno(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}

	scratch := t.TempDir()
	outpath := filepath.Join(scratch, "rendered.yaml")

	e := newTestEnv(t)

	tmpl := loadBuildinTemplateAs(t, "tmpl", nil)
	writer := loadBuildinWriteLocalAs(t, "writer")
	for _, s := range []*task.Spec{tmpl, writer} {
		if err := e.reg.Register(s); err != nil {
			t.Fatalf("reg.Register %s: %v", s.ID, err)
		}
		if err := e.engine.Register(s); err != nil {
			t.Fatalf("eng.Register %s: %v", s.ID, err)
		}
	}

	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "render-and-write", Name: "render-and-write", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Manual: true},
		Stages: []task.Stage{
			{
				Task: "tmpl",
				Overrides: &task.Overrides{
					Params: task.ParamOverrides{
						{Name: "template", Default: "rendered: hello-from-pipeline\n"},
					},
				},
			},
			{
				Task: "writer",
				Overrides: &task.Overrides{
					Params: task.ParamOverrides{
						{Name: "content", Default: "${input.output}"},
						{Name: "path", Default: outpath},
						{Name: "mode", Default: "0600"},
					},
					Fs: []task.FSEntry{{Path: scratch, Permission: "rw"}},
				},
			},
		},
	}
	if err := e.reg.Register(pipe); err != nil {
		t.Fatalf("reg.Register pipe: %v", err)
	}
	if err := e.engine.Register(pipe); err != nil {
		t.Fatalf("eng.Register pipe: %v", err)
	}

	parentRunID, err := e.engine.FireManual(context.Background(), "render-and-write", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}

	parent := waitForTerminal(t, e.engine, parentRunID, 30*time.Second)
	if parent.Status != registry.StatusSuccess {
		t.Fatalf("pipeline status = %q (reason=%q), want success", parent.Status, parent.FailureReason)
	}

	got, err := os.ReadFile(outpath)
	if err != nil {
		t.Fatalf("rendered file not found at %s: %v", outpath, err)
	}
	if want := "rendered: hello-from-pipeline\n"; string(got) != want {
		t.Errorf("rendered file content = %q, want %q", string(got), want)
	}

	kids, err := e.reg.ListChildren(context.Background(), parentRunID, 10)
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
}
