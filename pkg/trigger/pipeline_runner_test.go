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

// findPipelineRun polls for a kind=pipeline run of taskID and returns it once it
// reaches a terminal state (or fails the test on timeout).
func findRun(t *testing.T, env *testEnv, taskID string, wantKind string, timeout time.Duration) *registry.Run {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		runs, _ := env.reg.ListRuns(context.Background(), taskID, 5)
		for _, r := range runs {
			if wantKind != "" && r.Kind != wantKind {
				continue
			}
			if r.FinishedAt != nil {
				return r
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no terminal run for %q (kind=%q) within %v", taskID, wantKind, timeout)
	return nil
}

// TestPipelineWaitRunBlocksUntilDone asserts WaitRun on a pipeline's parent run
// blocks until the pipeline finishes and returns the terminal status + return
// value — i.e. the parent is a managed run (runDone), so dicode.run_task on a
// pipeline gets the real result rather than a racy "running"/nil.
func TestPipelineWaitRunBlocksUntilDone(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	env := newTestEnv(t)
	dir := t.TempDir()
	stage := writeTask(t, dir, "wstage", `export default async function main() { return "v" }`, task.TriggerConfig{Manual: true})
	if err := env.reg.Register(stage); err != nil {
		t.Fatal(err)
	}
	if err := env.engine.Register(stage); err != nil {
		t.Fatal(err)
	}
	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "wpipe", Name: "WP", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Manual: true},
		Stages:  []task.Stage{{Task: "wstage"}},
	}
	if err := env.reg.Register(pipe); err != nil {
		t.Fatal(err)
	}
	if err := env.engine.Register(pipe); err != nil {
		t.Fatalf("register pipeline: %v", err)
	}

	runID, err := env.engine.FireManual(context.Background(), "wpipe", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	res, err := env.engine.WaitRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("WaitRun: %v", err)
	}
	if res.Status != registry.StatusSuccess {
		t.Fatalf("WaitRun status = %q, want success (WaitRun should block until terminal)", res.Status)
	}
	if res.ReturnValue != "v" {
		t.Errorf("WaitRun return = %v, want \"v\"", res.ReturnValue)
	}
}

// TestPipelineKillRunCancelsStage asserts KillRun on a pipeline's parent run
// cancels the in-flight stage instead of leaving it running detached. The stage
// sleeps 10s; a working kill makes the pipeline terminate well before that.
func TestPipelineKillRunCancelsStage(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	env := newTestEnv(t)
	dir := t.TempDir()
	stage := writeTask(t, dir, "slowstage",
		`export default async function main() { await new Promise(r => setTimeout(r, 10000)); return "late" }`,
		task.TriggerConfig{Manual: true})
	if err := env.reg.Register(stage); err != nil {
		t.Fatal(err)
	}
	if err := env.engine.Register(stage); err != nil {
		t.Fatal(err)
	}
	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "killpipe", Name: "KP", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Manual: true},
		Stages:  []task.Stage{{Task: "slowstage"}},
	}
	if err := env.reg.Register(pipe); err != nil {
		t.Fatal(err)
	}
	if err := env.engine.Register(pipe); err != nil {
		t.Fatalf("register pipeline: %v", err)
	}

	runID, err := env.engine.FireManual(context.Background(), "killpipe", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	// Wait until the stage child run exists (pipeline is mid-stage).
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if kids, _ := env.reg.ListChildren(context.Background(), runID, 5); len(kids) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !env.engine.KillRun(runID) {
		t.Fatal("KillRun returned false; pipeline parent run is not cancellable")
	}
	// If kill works, the pipeline fails fast (well under the stage's 10s sleep).
	parent := waitForTerminal(t, env.engine, runID, 8*time.Second)
	if parent.Status == registry.StatusSuccess {
		t.Fatalf("killed pipeline ended success; expected failure")
	}
}

// TestChainFiresPipeline asserts a pipeline with trigger.chain.from: <task> fires
// when that upstream task completes successfully.
func TestChainFiresPipeline(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	env := newTestEnv(t)
	dir := t.TempDir()

	stage := writeTask(t, dir, "pstage", `export default async function main() { return "ok" }`, task.TriggerConfig{Manual: true})
	upstream := writeTask(t, dir, "up", `export default async function main() { return "done" }`, task.TriggerConfig{Manual: true})
	for _, s := range []*task.Spec{stage, upstream} {
		if err := env.reg.Register(s); err != nil {
			t.Fatal(err)
		}
		if err := env.engine.Register(s); err != nil {
			t.Fatal(err)
		}
	}
	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "chained-pipe", Name: "CP", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Chain: &task.ChainTrigger{From: "up", On: "success"}},
		Stages:  []task.Stage{{Task: "pstage"}},
	}
	if err := env.reg.Register(pipe); err != nil {
		t.Fatal(err)
	}
	if err := env.engine.Register(pipe); err != nil {
		t.Fatalf("register pipeline: %v", err)
	}

	if _, err := env.engine.FireManual(context.Background(), "up", nil); err != nil {
		t.Fatalf("FireManual up: %v", err)
	}

	run := findRun(t, env, "chained-pipe", registry.RunKindPipeline, 30*time.Second)
	if run.Status != registry.StatusSuccess {
		t.Errorf("chained pipeline status = %q, want success", run.Status)
	}
	if run.TriggerSource != registry.TriggerChain {
		t.Errorf("chained pipeline source = %q, want %q", run.TriggerSource, registry.TriggerChain)
	}
}

// TestPipelineFiresChain asserts a downstream kind: Task chains from a pipeline's
// overall outcome (pipeline-as-chain-source).
func TestPipelineFiresChain(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	env := newTestEnv(t)
	dir := t.TempDir()

	stage := writeTask(t, dir, "pstage2", `export default async function main() { return "ok" }`, task.TriggerConfig{Manual: true})
	if err := env.reg.Register(stage); err != nil {
		t.Fatal(err)
	}
	if err := env.engine.Register(stage); err != nil {
		t.Fatal(err)
	}
	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "src-pipe", Name: "SP", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Manual: true},
		Stages:  []task.Stage{{Task: "pstage2"}},
	}
	if err := env.reg.Register(pipe); err != nil {
		t.Fatal(err)
	}
	if err := env.engine.Register(pipe); err != nil {
		t.Fatalf("register pipeline: %v", err)
	}
	downstream := writeTask(t, dir, "after-pipe", `export default async function main() { return "after" }`,
		task.TriggerConfig{Chain: &task.ChainTrigger{From: "src-pipe", On: "success"}})
	if err := env.reg.Register(downstream); err != nil {
		t.Fatal(err)
	}
	if err := env.engine.Register(downstream); err != nil {
		t.Fatal(err)
	}

	if _, err := env.engine.FireManual(context.Background(), "src-pipe", nil); err != nil {
		t.Fatalf("FireManual src-pipe: %v", err)
	}

	run := findRun(t, env, "after-pipe", registry.RunKindTask, 30*time.Second)
	if run.TriggerSource != registry.TriggerChain {
		t.Errorf("downstream source = %q, want %q (chained from pipeline outcome)", run.TriggerSource, registry.TriggerChain)
	}
}
