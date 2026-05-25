package trigger

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
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

// findStageChild polls a pipeline parent run's children for the named stage task
// and returns it once observed in any state (or fails on timeout).
func findStageChild(t *testing.T, env *testEnv, parentRunID, stageTaskID string, want string, timeout time.Duration) *registry.Run {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		kids, _ := env.reg.ListChildren(context.Background(), parentRunID, 20)
		for _, c := range kids {
			if c.TaskID != stageTaskID {
				continue
			}
			if want == "" || c.Status == want {
				return c
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("stage child %q (want status %q) not observed under %s", stageTaskID, want, timeout)
	return nil
}

// TestPipelineDaemonTerminalStage asserts that a pipeline whose terminal stage
// resolves to a trigger.daemon: true Task does NOT finish 'success' when the
// daemon starts: it stays 'running' for the daemon's lifetime and finishes with
// the daemon run's *actual* terminal status.
//
// Two behaviours are checked:
//   - lifetime: while the daemon stage child is 'running', the pipeline parent
//     is still 'running' (gated on the observable child-run state, not a sleep);
//   - status fidelity: when the daemon run is killed (operator-style), the
//     pipeline finishes with the daemon's terminal status ('cancelled'). The
//     pre-Task-18 generic dispatchStage path would coerce any non-success
//     terminal into a wrapped 'failure', so this distinguishes the new code.
func TestPipelineDaemonTerminalStage(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	env := newTestEnv(t)
	dir := t.TempDir()

	stageA := writeTask(t, dir, "dts-a",
		`export default async function main() { return "from-a" }`,
		task.TriggerConfig{Manual: true})
	// Terminal stage is a daemon: it stays up until killed. restart:never so a
	// kill doesn't loop.
	stageB := writeTask(t, dir, "dts-b",
		`export default async function main() { while (true) { await new Promise(r => setTimeout(r, 200)); } }`,
		task.TriggerConfig{Daemon: true, Restart: "never"})
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
		ID: "dts-pipe", Name: "DTS", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Manual: true},
		Stages:  []task.Stage{{Task: "dts-a"}, {Task: "dts-b"}},
	}
	if err := env.reg.Register(pipe); err != nil {
		t.Fatalf("reg.Register pipe: %v", err)
	}
	if err := env.engine.Register(pipe); err != nil {
		t.Fatalf("eng.Register pipe: %v", err)
	}

	parentRunID, err := env.engine.FireManual(context.Background(), "dts-pipe", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}

	// Wait until the daemon terminal stage child run is up and 'running'.
	daemonChild := findStageChild(t, env, parentRunID, "dts-b", registry.StatusRunning, 20*time.Second)

	// While the daemon stage is running, the pipeline parent must NOT have
	// finished — it tracks the daemon's lifetime.
	parent, err := env.engine.registry.GetRun(context.Background(), parentRunID)
	if err != nil {
		t.Fatalf("GetRun parent: %v", err)
	}
	if parent.Status != registry.StatusRunning {
		t.Fatalf("pipeline finished %q while daemon terminal stage still running; want 'running'", parent.Status)
	}

	// Kill the daemon stage run (operator-style). The pipeline must finish with
	// the daemon run's *actual* terminal status (cancelled), not a coerced
	// failure.
	if !env.engine.KillRun(daemonChild.ID) {
		t.Fatalf("KillRun(daemonChild) returned false; daemon stage run not cancellable")
	}

	final := waitForTerminal(t, env.engine, parentRunID, 20*time.Second)
	if final.Status != registry.StatusCancelled {
		t.Fatalf("pipeline final status = %q (reason=%q), want 'cancelled' (daemon run was killed)", final.Status, final.FailureReason)
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

// countStageChildren returns how many of a pipeline parent run's children are
// runs of stageTaskID.
func countStageChildren(t *testing.T, env *testEnv, parentRunID, stageTaskID string) int {
	t.Helper()
	kids, _ := env.reg.ListChildren(context.Background(), parentRunID, 100)
	n := 0
	for _, c := range kids {
		if c.TaskID == stageTaskID {
			n++
		}
	}
	return n
}

// TestPipelineStageRerun is the mid-pipeline stage re-fire propagation test
// (Task 19). A 3-stage pipeline runs to its terminal daemon stage and stays
// live. An operator then re-fires stage 0's underlying Task standalone. The
// engine must replay the descendant stages [1..terminal] with fresh ${input}
// and restart the terminal daemon — observed as: a SECOND run of the descendant
// stage-1 task appears as a pipeline child, AND a SECOND run of the terminal
// daemon stage appears (the restart).
//
// Determinism: every assertion gates on observable child-run counts going from
// 1 to 2 (not sleeps). The daemon body blocks until killed, so the only way a
// second daemon-stage child appears is the re-fire's kill+restart.
func TestPipelineStageRerun(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	env := newTestEnv(t)
	dir := t.TempDir()

	stage0 := writeTask(t, dir, "rr-0",
		`export default async function main() { return "gen" }`,
		task.TriggerConfig{Manual: true})
	stage1 := writeTask(t, dir, "rr-1",
		`export default async function main({ params }) { return await params.get("content") }`,
		task.TriggerConfig{Manual: true})
	// Terminal daemon stage blocks until killed; restart:never so a kill
	// doesn't engage the daemon auto-restart path (this stage is a pipeline
	// child, not a registered daemon, so that path is inert anyway).
	stage2 := writeTask(t, dir, "rr-2",
		`export default async function main() { while (true) { await new Promise(r => setTimeout(r, 200)); } }`,
		task.TriggerConfig{Daemon: true, Restart: "never"})
	for _, s := range []*task.Spec{stage0, stage1, stage2} {
		if err := env.reg.Register(s); err != nil {
			t.Fatalf("reg.Register %s: %v", s.ID, err)
		}
		if err := env.engine.Register(s); err != nil {
			t.Fatalf("eng.Register %s: %v", s.ID, err)
		}
	}

	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "rr-pipe", Name: "RR", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Manual: true},
		Stages: []task.Stage{
			{Task: "rr-0"},
			{Task: "rr-1", Overrides: &task.Overrides{
				Params: task.ParamOverrides{{Name: "content", Default: "${input.output}"}}}},
			{Task: "rr-2"},
		},
	}
	if err := env.reg.Register(pipe); err != nil {
		t.Fatalf("reg.Register pipe: %v", err)
	}
	if err := env.engine.Register(pipe); err != nil {
		t.Fatalf("eng.Register pipe: %v", err)
	}

	parentRunID, err := env.engine.FireManual(context.Background(), "rr-pipe", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}

	// Wait until the terminal daemon stage is up: the pipeline is live.
	findStageChild(t, env, parentRunID, "rr-2", registry.StatusRunning, 20*time.Second)
	// Sanity: exactly one run each of the descendant + daemon stages so far.
	if got := countStageChildren(t, env, parentRunID, "rr-1"); got != 1 {
		t.Fatalf("before re-fire: rr-1 child count = %d, want 1", got)
	}
	if got := countStageChildren(t, env, parentRunID, "rr-2"); got != 1 {
		t.Fatalf("before re-fire: rr-2 child count = %d, want 1", got)
	}

	// Operator re-fires stage 0's underlying Task standalone. On success its
	// FireChain hook drives handlePipelineStageRerun → replay descendants +
	// restart the terminal daemon.
	if _, err := env.engine.FireManual(context.Background(), "rr-0", nil); err != nil {
		t.Fatalf("FireManual rr-0 (re-fire): %v", err)
	}

	// Descendant stage-1 must re-run (a 2nd pipeline-child run appears).
	waitUntil(t, 25*time.Second, func() bool {
		return countStageChildren(t, env, parentRunID, "rr-1") >= 2
	}, "descendant stage rr-1 was not re-run after stage-0 re-fire")

	// Terminal daemon must restart (a 2nd daemon-stage child run appears, and
	// it reaches 'running').
	waitUntil(t, 25*time.Second, func() bool {
		if countStageChildren(t, env, parentRunID, "rr-2") < 2 {
			return false
		}
		// At least one rr-2 child should be 'running' (the restarted daemon).
		kids, _ := env.reg.ListChildren(context.Background(), parentRunID, 100)
		for _, c := range kids {
			if c.TaskID == "rr-2" && c.Status == registry.StatusRunning {
				return true
			}
		}
		return false
	}, "terminal daemon stage rr-2 did not restart after stage-0 re-fire")

	// The pipeline parent must still be running across the restart (its
	// lifetime tracks the restarted daemon, not the killed one).
	parent, err := env.engine.registry.GetRun(context.Background(), parentRunID)
	if err != nil {
		t.Fatalf("GetRun parent: %v", err)
	}
	if parent.Status != registry.StatusRunning {
		t.Fatalf("pipeline parent status = %q after restart, want 'running'", parent.Status)
	}

	// Cleanup: kill the pipeline so the daemon subprocess doesn't linger.
	env.engine.KillRun(parentRunID)
	waitForTerminal(t, env.engine, parentRunID, 15*time.Second)
}

// TestPipelineParentKillDuringRestart covers the parent-kill-mid-restart window
// that TestPipelineStageRerun misses (#344, HIGH). A live pipeline with a daemon
// terminal stage is restarted by a mid-pipeline stage re-fire; the operator
// KillRun's the pipeline PARENT while the restart handshake is in flight — old
// daemon run already killed, the freshly-fired daemon run not yet published to
// the runner's wait loop.
//
// Determinism: the runStartedHook fires synchronously inside fireStageRaw, so
// when it observes the 2nd start of the terminal daemon task (the restart's new
// run), propagatePipelineStageRerun is still blocked inside fireStageRaw and has
// NOT reached its publish critical section. KillRun(parent) from the hook lands
// runCtx cancellation squarely in the mid-restart window. The publish path must
// then refuse to adopt the fresh daemon (runCtx cancelled), kill it, and let the
// pipeline finish 'cancelled'.
//
// Invariants asserted after the parent kill:
//
//	(a) no daemon-stage child run remains 'running' (no orphaned subprocess);
//	(b) the pipeline parent ends non-'running' (cancelled).
func TestPipelineParentKillDuringRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	env := newTestEnv(t)
	dir := t.TempDir()

	stage0 := writeTask(t, dir, "pk-0",
		`export default async function main() { return "gen" }`,
		task.TriggerConfig{Manual: true})
	stage1 := writeTask(t, dir, "pk-1",
		`export default async function main({ params }) { return await params.get("content") }`,
		task.TriggerConfig{Manual: true})
	// Terminal daemon stage blocks until killed; restart:never so a kill
	// doesn't engage the daemon auto-restart path.
	stage2 := writeTask(t, dir, "pk-2",
		`export default async function main() { while (true) { await new Promise(r => setTimeout(r, 200)); } }`,
		task.TriggerConfig{Daemon: true, Restart: "never"})
	for _, s := range []*task.Spec{stage0, stage1, stage2} {
		if err := env.reg.Register(s); err != nil {
			t.Fatalf("reg.Register %s: %v", s.ID, err)
		}
		if err := env.engine.Register(s); err != nil {
			t.Fatalf("eng.Register %s: %v", s.ID, err)
		}
	}

	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "pk-pipe", Name: "PK", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Manual: true},
		Stages: []task.Stage{
			{Task: "pk-0"},
			{Task: "pk-1", Overrides: &task.Overrides{
				Params: task.ParamOverrides{{Name: "content", Default: "${input.output}"}}}},
			{Task: "pk-2"},
		},
	}
	if err := env.reg.Register(pipe); err != nil {
		t.Fatalf("reg.Register pipe: %v", err)
	}
	if err := env.engine.Register(pipe); err != nil {
		t.Fatalf("eng.Register pipe: %v", err)
	}

	parentRunID, err := env.engine.FireManual(context.Background(), "pk-pipe", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}

	// Install the kill-during-restart trap: on the 2nd start of the terminal
	// daemon task (= the restart's freshly-fired run), KillRun the pipeline
	// parent. The hook runs synchronously inside fireStageRaw, before the
	// runner's wait loop can adopt the new run — i.e. inside the restart window.
	var daemonStarts int32
	killed := make(chan struct{})
	var killOnce sync.Once
	env.engine.SetRunStartedHook(func(taskID, runID, triggerSource string) {
		if taskID != "pk-2" {
			return
		}
		if atomic.AddInt32(&daemonStarts, 1) == 2 {
			env.engine.KillRun(parentRunID)
			killOnce.Do(func() { close(killed) })
		}
	})

	// Wait until the terminal daemon stage is up: the pipeline is live.
	findStageChild(t, env, parentRunID, "pk-2", registry.StatusRunning, 20*time.Second)

	// Operator re-fires stage 0 standalone → handlePipelineStageRerun replays
	// descendants + restarts the terminal daemon. The restart's new run start
	// trips the hook above, which KillRun's the parent mid-restart.
	if _, err := env.engine.FireManual(context.Background(), "pk-0", nil); err != nil {
		t.Fatalf("FireManual pk-0 (re-fire): %v", err)
	}

	// The hook must fire (the restart must reach the new-run-start point).
	select {
	case <-killed:
	case <-time.After(25 * time.Second):
		t.Fatal("restart never reached the new-daemon-run start; window not exercised")
	}

	// (b) The pipeline parent must end non-'running' (cancelled), not wedge.
	final := waitForTerminal(t, env.engine, parentRunID, 20*time.Second)
	if final.Status == registry.StatusRunning {
		t.Fatalf("pipeline parent wedged 'running' after parent KillRun during restart")
	}
	if final.Status != registry.StatusCancelled {
		t.Fatalf("pipeline final status = %q (reason=%q), want 'cancelled'", final.Status, final.FailureReason)
	}

	// (a) No daemon-stage child run may remain 'running' — neither the killed
	// old run nor the freshly-fired-but-unadopted new run. Poll: the new run's
	// kill is async (KillRun cancels its ctx; the run drains to a terminal).
	waitUntil(t, 20*time.Second, func() bool {
		kids, _ := env.reg.ListChildren(context.Background(), parentRunID, 100)
		for _, c := range kids {
			if c.TaskID == "pk-2" && c.Status == registry.StatusRunning {
				return false
			}
		}
		return true
	}, "a terminal daemon stage child remained 'running' after parent KillRun during restart")
}

// TestPipelineStageDaemonDoesNotFlipStandaloneDaemonState covers the
// daemon-state cross-contamination bug (#344, LOW security): a task that is BOTH
// a registered standalone daemon AND a pipeline's terminal stage. Killing the
// pipeline-stage run (as a mid-pipeline restart does) must NOT route through the
// standalone-daemon lifecycle hook (onDaemonRunFinished) and flip the standalone
// daemon's global DaemonState.
//
// Setup: register daemon task "shared" — registration auto-starts it standalone
// (DaemonState→Running, its own standalone run). Then fire a pipeline whose
// terminal stage is "shared"; that dispatches a SEPARATE pipeline-stage run of
// the same task. KillRun the pipeline-stage run. With the bug,
// onDaemonRunFinished(shared, pipelineStageRunID) fires and flips
// DaemonState(shared) to Stopped/Crashed even though the standalone daemon run
// is untouched. The fix suppresses the hook for pipeline-stage runs, so the
// standalone daemon's state stays Running.
func TestPipelineStageDaemonDoesNotFlipStandaloneDaemonState(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	env := newTestEnv(t)
	dir := t.TempDir()

	// "shared" is a daemon that loops forever; restart:never so a kill doesn't
	// auto-restart it.
	shared := writeTask(t, dir, "shared",
		`export default async function main() { while (true) { await new Promise(r => setTimeout(r, 200)); } }`,
		task.TriggerConfig{Daemon: true, Restart: "never"})
	if err := env.reg.Register(shared); err != nil {
		t.Fatalf("reg.Register shared: %v", err)
	}
	if err := env.engine.Register(shared); err != nil {
		t.Fatalf("eng.Register shared: %v", err)
	}

	// Registration auto-starts the STANDALONE daemon. Wait until it's Running.
	waitUntil(t, 20*time.Second, func() bool {
		return env.engine.DaemonState("shared") == DaemonRunning
	}, "standalone daemon 'shared' did not reach DaemonRunning after registration")

	// Record the standalone daemon's own run ID so we can tell it apart from the
	// pipeline-stage run of the same task.
	env.engine.daemonMu.Lock()
	standaloneRunID := env.engine.daemonRuns["shared"]
	env.engine.daemonMu.Unlock()
	if standaloneRunID == "" {
		t.Fatalf("no standalone daemon run recorded for 'shared'")
	}

	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "sd-pipe", Name: "SD", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Manual: true},
		Stages:  []task.Stage{{Task: "shared"}},
	}
	if err := env.reg.Register(pipe); err != nil {
		t.Fatalf("reg.Register pipe: %v", err)
	}
	if err := env.engine.Register(pipe); err != nil {
		t.Fatalf("eng.Register pipe: %v", err)
	}

	parentRunID, err := env.engine.FireManual(context.Background(), "sd-pipe", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}

	// Find the pipeline-stage run of "shared" (a child of the pipeline parent,
	// distinct from the standalone run) once it's running.
	stageChild := findStageChild(t, env, parentRunID, "shared", registry.StatusRunning, 20*time.Second)
	if stageChild.ID == standaloneRunID {
		t.Fatalf("pipeline-stage run ID collided with standalone run ID %s", standaloneRunID)
	}

	// Kill the PIPELINE-STAGE run (this is what a mid-pipeline restart does to
	// the old terminal-daemon stage run). It must not disturb the standalone
	// daemon's lifecycle/state.
	if !env.engine.KillRun(stageChild.ID) {
		t.Fatalf("KillRun(stageChild) returned false")
	}

	// Wait for the pipeline-stage run to actually terminate, so the
	// onDaemonRunFinished hook (if it were going to fire) would have fired.
	if _, werr := env.engine.WaitRun(context.Background(), stageChild.ID); werr != nil {
		t.Fatalf("WaitRun(stageChild): %v", werr)
	}

	// The standalone daemon must remain Running: its state was NOT flipped by
	// the pipeline-stage run finishing. Poll briefly to give any spurious async
	// hook a chance to mis-fire (it must not).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := env.engine.DaemonState("shared"); got != DaemonRunning {
			t.Fatalf("standalone daemon 'shared' state flipped to %q after pipeline-stage run killed; want still %q", got, DaemonRunning)
		}
		time.Sleep(50 * time.Millisecond)
	}
	// And its standalone run row must be untouched (still the same run, not
	// removed from daemonRuns).
	env.engine.daemonMu.Lock()
	stillTracked := env.engine.daemonRuns["shared"]
	env.engine.daemonMu.Unlock()
	if stillTracked != standaloneRunID {
		t.Fatalf("standalone daemon run tracking changed: %q != %q", stillTracked, standaloneRunID)
	}

	// Cleanup: stop the standalone daemon subprocess.
	env.engine.KillRun(standaloneRunID)
	// And drain the pipeline (its terminal stage was killed → pipeline finishes).
	waitForTerminal(t, env.engine, parentRunID, 15*time.Second)
}

// TestPipelineStage0ReceivesTriggerInput asserts that a manual fire with params
// seeds stage 0's ${input.params.X} and ${input.output} references (#350).
func TestPipelineStage0ReceivesTriggerInput(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	env := newTestEnv(t)
	dir := t.TempDir()

	// stage-seed: receives a param "greeting" wired from trigger params, echoes
	// it back as the return value.
	stageSeed := writeTask(t, dir, "seed-stage",
		`export default async function main({ params }) { return await params.get("greeting") }`,
		task.TriggerConfig{Manual: true})
	for _, s := range []*task.Spec{stageSeed} {
		if err := env.reg.Register(s); err != nil {
			t.Fatalf("reg.Register %s: %v", s.ID, err)
		}
		if err := env.engine.Register(s); err != nil {
			t.Fatalf("eng.Register %s: %v", s.ID, err)
		}
	}

	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "seed-pipe", Name: "SP2", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Manual: true},
		Stages: []task.Stage{
			{Task: "seed-stage", Overrides: &task.Overrides{
				Params: task.ParamOverrides{
					{Name: "greeting", Default: "${input.params.greeting}"},
				},
			}},
		},
	}
	if err := env.reg.Register(pipe); err != nil {
		t.Fatalf("reg.Register pipe: %v", err)
	}
	if err := env.engine.Register(pipe); err != nil {
		t.Fatalf("eng.Register pipe: %v", err)
	}

	parentRunID, err := env.engine.FireManual(context.Background(), "seed-pipe",
		map[string]string{"greeting": "hello-trigger"})
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}

	res := waitForTerminal(t, env.engine, parentRunID, 30*time.Second)
	if res.Status != registry.StatusSuccess {
		t.Fatalf("pipeline status = %q (reason=%q), want success", res.Status, res.FailureReason)
	}
	// The pipeline's return value is stage-0's return, which should be the
	// trigger param value threaded via ${input.params.greeting}.
	parent, err := env.engine.WaitRun(context.Background(), parentRunID)
	if err != nil {
		t.Fatalf("WaitRun: %v", err)
	}
	if parent.ReturnValue != "hello-trigger" {
		t.Errorf("pipeline return = %v, want \"hello-trigger\" (trigger param not seeded to stage 0)", parent.ReturnValue)
	}
}

// TestPipelineStage0CronEmptyInput asserts that a cron/empty-trigger fire seeds
// an empty InputContext and a ${input.params.X} ref on stage 0 fails with
// ErrInputUnavailable (not silently empty string).
func TestPipelineStage0CronEmptyInput(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	env := newTestEnv(t)
	dir := t.TempDir()

	// stage-fail: tries to read ${input.params.key} — should receive
	// ErrInputUnavailable, causing the stage + pipeline to fail.
	stageNoInput := writeTask(t, dir, "noinput-stage",
		`export default async function main({ params }) { return await params.get("key") }`,
		task.TriggerConfig{Manual: true})
	if err := env.reg.Register(stageNoInput); err != nil {
		t.Fatalf("reg.Register %s: %v", stageNoInput.ID, err)
	}
	if err := env.engine.Register(stageNoInput); err != nil {
		t.Fatalf("eng.Register %s: %v", stageNoInput.ID, err)
	}

	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "cron-pipe", Name: "CP2", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Manual: true},
		Stages: []task.Stage{
			{Task: "noinput-stage", Overrides: &task.Overrides{
				Params: task.ParamOverrides{
					{Name: "key", Default: "${input.params.key}"},
				},
			}},
		},
	}
	if err := env.reg.Register(pipe); err != nil {
		t.Fatalf("reg.Register pipe: %v", err)
	}
	if err := env.engine.Register(pipe); err != nil {
		t.Fatalf("eng.Register pipe: %v", err)
	}

	// Fire with NO params (simulates cron — empty opts).
	parentRunID, err := env.engine.FireManual(context.Background(), "cron-pipe", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}

	res := waitForTerminal(t, env.engine, parentRunID, 30*time.Second)
	// The pipeline must FAIL (not succeed with empty string) because no
	// trigger params were provided — ErrInputUnavailable.
	if res.Status == registry.StatusSuccess {
		t.Fatal("pipeline should fail when stage 0 ${input.params.X} has no trigger input, but succeeded")
	}
	// Confirm the failure is specifically the unresolved-input path, not a
	// Deno crash or registration error — FailureReason must mention "${input".
	if !strings.Contains(res.FailureReason, "${input") {
		t.Errorf("pipeline failure reason %q does not mention ${input}; want ErrInputUnavailable path, not a spurious error", res.FailureReason)
	}
}

// TestPipelineWebhookStage0Input asserts that a webhook POST body flows through
// as the trigger payload for stage 0 (${input.params.X} from webhook body).
func TestPipelineWebhookStage0Input(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	env := newTestEnv(t)
	dir := t.TempDir()

	stageWH := writeTask(t, dir, "wh-stage",
		`export default async function main({ params }) { return await params.get("event") }`,
		task.TriggerConfig{Manual: true})
	if err := env.reg.Register(stageWH); err != nil {
		t.Fatalf("reg.Register %s: %v", stageWH.ID, err)
	}
	if err := env.engine.Register(stageWH); err != nil {
		t.Fatalf("eng.Register %s: %v", stageWH.ID, err)
	}

	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "wh-pipe", Name: "WHP", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Webhook: "/hooks/wh-pipe"},
		Stages: []task.Stage{
			{Task: "wh-stage", Overrides: &task.Overrides{
				Params: task.ParamOverrides{
					{Name: "event", Default: "${input.params.event}"},
				},
			}},
		},
	}
	if err := env.reg.Register(pipe); err != nil {
		t.Fatalf("reg.Register pipe: %v", err)
	}
	if err := env.engine.Register(pipe); err != nil {
		t.Fatalf("eng.Register pipe: %v", err)
	}

	// POST a JSON body to the pipeline webhook — body should seed stage 0.
	handler := env.engine.WebhookHandler()
	body := []byte(`{"event":"push"}`)
	req := httptest.NewRequest("POST", "/hooks/wh-pipe", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("webhook POST status = %d, want 200: %s", w.Code, w.Body.String())
	}

	runID := w.Header().Get("X-Run-Id")
	if runID == "" {
		t.Fatal("no X-Run-Id in response")
	}

	res := waitForTerminal(t, env.engine, runID, 30*time.Second)
	if res.Status != registry.StatusSuccess {
		t.Fatalf("pipeline status = %q (reason=%q), want success", res.Status, res.FailureReason)
	}
	parent, err := env.engine.WaitRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("WaitRun: %v", err)
	}
	if parent.ReturnValue != "push" {
		t.Errorf("pipeline return = %v, want \"push\" (webhook body not seeded to stage 0)", parent.ReturnValue)
	}
}

// TestPipelineStage0ManualOutputRef asserts that ${input.output.<name>} on stage 0
// resolves to the named manual-trigger param value (#350). This covers the case
// where opts.Input is nil (manual fire) but opts.Params is non-empty: firePipeline
// must promote params to a map[string]any so ${input.output} and
// ${input.output.<field>} resolve correctly, not just ${input.params.<name>}.
func TestPipelineStage0ManualOutputRef(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	env := newTestEnv(t)
	dir := t.TempDir()

	// stage echoes its "val" param (wired via ${input.output.greeting}).
	stageOut := writeTask(t, dir, "out-stage",
		`export default async function main({ params }) { return await params.get("val") }`,
		task.TriggerConfig{Manual: true})
	if err := env.reg.Register(stageOut); err != nil {
		t.Fatalf("reg.Register %s: %v", stageOut.ID, err)
	}
	if err := env.engine.Register(stageOut); err != nil {
		t.Fatalf("eng.Register %s: %v", stageOut.ID, err)
	}

	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "out-pipe", Name: "OP", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Manual: true},
		Stages: []task.Stage{
			{Task: "out-stage", Overrides: &task.Overrides{
				Params: task.ParamOverrides{
					// ${input.output.greeting} — fields of the promoted params map.
					{Name: "val", Default: "${input.output.greeting}"},
				},
			}},
		},
	}
	if err := env.reg.Register(pipe); err != nil {
		t.Fatalf("reg.Register pipe: %v", err)
	}
	if err := env.engine.Register(pipe); err != nil {
		t.Fatalf("eng.Register pipe: %v", err)
	}

	parentRunID, err := env.engine.FireManual(context.Background(), "out-pipe",
		map[string]string{"greeting": "hi-from-output"})
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}

	res := waitForTerminal(t, env.engine, parentRunID, 30*time.Second)
	if res.Status != registry.StatusSuccess {
		t.Fatalf("pipeline status = %q (reason=%q), want success", res.Status, res.FailureReason)
	}
	parent, err := env.engine.WaitRun(context.Background(), parentRunID)
	if err != nil {
		t.Fatalf("WaitRun: %v", err)
	}
	if parent.ReturnValue != "hi-from-output" {
		t.Errorf("pipeline return = %v, want \"hi-from-output\" (${input.output.<name>} on stage 0 not resolved from manual params)", parent.ReturnValue)
	}
}

// TestPipelineStage0CronRealDispatch asserts that firing a pipeline via the
// actual cron dispatch path (registerPipelineCron → fireKinded with TriggerCron,
// empty RunOptions) seeds stage 0 with a nil/empty InputContext and that a
// ${input.params.X} ref fails loudly (ErrInputUnavailable), i.e. the pipeline
// ends in a failure status. This exercises the real cron code path rather than
// simulating it via FireManual.
func TestPipelineStage0CronRealDispatch(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	env := newTestEnv(t)
	dir := t.TempDir()

	// stage-cron: tries to read ${input.params.key} — must fail loud when no
	// trigger payload is present (cron fires with empty RunOptions).
	stageCron := writeTask(t, dir, "cron-real-stage",
		`export default async function main({ params }) { return await params.get("key") }`,
		task.TriggerConfig{Manual: true})
	if err := env.reg.Register(stageCron); err != nil {
		t.Fatalf("reg.Register %s: %v", stageCron.ID, err)
	}
	if err := env.engine.Register(stageCron); err != nil {
		t.Fatalf("eng.Register %s: %v", stageCron.ID, err)
	}

	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "cron-real-pipe", Name: "CRP", Subtype: "sequential", Enabled: true,
		// Cron field is set but we invoke the dispatch path directly below
		// rather than waiting on a wall-clock tick.
		Trigger: task.PipelineTrigger{Cron: "0 0 1 1 *"},
		Stages: []task.Stage{
			{Task: "cron-real-stage", Overrides: &task.Overrides{
				Params: task.ParamOverrides{
					{Name: "key", Default: "${input.params.key}"},
				},
			}},
		},
	}
	if err := env.reg.Register(pipe); err != nil {
		t.Fatalf("reg.Register pipe: %v", err)
	}
	if err := env.engine.Register(pipe); err != nil {
		t.Fatalf("eng.Register pipe: %v", err)
	}

	// Invoke the cron dispatch path directly: retrieve the kinded task and fire
	// it through fireKinded with TriggerCron + empty RunOptions — exactly what
	// registerPipelineCron's callback does on each tick.
	k, ok := env.engine.registry.GetKinded("cron-real-pipe")
	if !ok {
		t.Fatal("cron-real-pipe not in registry")
	}
	parentRunID, err := env.engine.fireKinded(context.Background(), k,
		pkgruntime.RunOptions{}, registry.TriggerCron)
	if err != nil {
		t.Fatalf("fireKinded(cron): %v", err)
	}

	res := waitForTerminal(t, env.engine, parentRunID, 30*time.Second)
	// Cron fires with no params → stage-0 ${input.params.key} must fail loud
	// (ErrInputUnavailable), causing the pipeline to end in failure.
	if res.Status == registry.StatusSuccess {
		t.Fatal("cron-dispatched pipeline should fail when stage 0 has ${input.params.X} but no trigger payload, but succeeded")
	}
	if res.TriggerSource != registry.TriggerCron {
		t.Errorf("trigger_source = %q, want %q", res.TriggerSource, registry.TriggerCron)
	}
	// Confirm the failure is specifically the unresolved-input path, not a
	// Deno crash or registration error — FailureReason must mention "${input".
	if !strings.Contains(res.FailureReason, "${input") {
		t.Errorf("pipeline failure reason %q does not mention ${input}; want ErrInputUnavailable path, not a spurious error", res.FailureReason)
	}
}

// TestPipelineChainStage0Input asserts that chain.params from a chain-triggered
// pipeline fire flow through as the trigger payload for stage 0 (#350).
func TestPipelineChainStage0Input(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	env := newTestEnv(t)
	dir := t.TempDir()

	upstream := writeTask(t, dir, "chain-up",
		`export default async function main() { return "upstream-done" }`,
		task.TriggerConfig{Manual: true})
	stageChain := writeTask(t, dir, "chain-stage",
		`export default async function main({ params }) { return await params.get("msg") }`,
		task.TriggerConfig{Manual: true})
	for _, s := range []*task.Spec{upstream, stageChain} {
		if err := env.reg.Register(s); err != nil {
			t.Fatalf("reg.Register %s: %v", s.ID, err)
		}
		if err := env.engine.Register(s); err != nil {
			t.Fatalf("eng.Register %s: %v", s.ID, err)
		}
	}

	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "chain-pipe2", Name: "CP3", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Chain: &task.ChainTrigger{
			From:   "chain-up",
			On:     "success",
			Params: map[string]interface{}{"msg": "chain-hello"},
		}},
		Stages: []task.Stage{
			{Task: "chain-stage", Overrides: &task.Overrides{
				Params: task.ParamOverrides{
					{Name: "msg", Default: "${input.params.msg}"},
				},
			}},
		},
	}
	if err := env.reg.Register(pipe); err != nil {
		t.Fatalf("reg.Register pipe: %v", err)
	}
	if err := env.engine.Register(pipe); err != nil {
		t.Fatalf("eng.Register pipe: %v", err)
	}

	if _, err := env.engine.FireManual(context.Background(), "chain-up", nil); err != nil {
		t.Fatalf("FireManual chain-up: %v", err)
	}

	run := findRun(t, env, "chain-pipe2", registry.RunKindPipeline, 30*time.Second)
	if run.Status != registry.StatusSuccess {
		t.Fatalf("chained pipeline status = %q, want success", run.Status)
	}
	parent, err := env.engine.WaitRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("WaitRun: %v", err)
	}
	if parent.ReturnValue != "chain-hello" {
		t.Errorf("pipeline return = %v, want \"chain-hello\" (chain.params not seeded to stage 0)", parent.ReturnValue)
	}
}

// TestPipelineWebhookStage0GETQuery (FIX F) asserts that a webhook GET request
// with query params seeds stage 0 via ${input.output.<field>} (the query map
// is threaded as the trigger Input, so ${input.output.X} resolves to the query
// param named X).
func TestPipelineWebhookStage0GETQuery(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	env := newTestEnv(t)
	dir := t.TempDir()

	stageGET := writeTask(t, dir, "get-stage",
		`export default async function main({ params }) { return await params.get("ref") }`,
		task.TriggerConfig{Manual: true})
	if err := env.reg.Register(stageGET); err != nil {
		t.Fatalf("reg.Register %s: %v", stageGET.ID, err)
	}
	if err := env.engine.Register(stageGET); err != nil {
		t.Fatalf("eng.Register %s: %v", stageGET.ID, err)
	}

	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "get-pipe", Name: "GP", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Webhook: "/hooks/get-pipe"},
		Stages: []task.Stage{
			{Task: "get-stage", Overrides: &task.Overrides{
				Params: task.ParamOverrides{
					// GET query params are decoded into a map and set as Input,
					// so ${input.output.<field>} resolves the named query param.
					{Name: "ref", Default: "${input.output.branch}"},
				},
			}},
		},
	}
	if err := env.reg.Register(pipe); err != nil {
		t.Fatalf("reg.Register pipe: %v", err)
	}
	if err := env.engine.Register(pipe); err != nil {
		t.Fatalf("eng.Register pipe: %v", err)
	}

	handler := env.engine.WebhookHandler()
	req := httptest.NewRequest("GET", "/hooks/get-pipe?branch=main", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("webhook GET status = %d, want 200: %s", w.Code, w.Body.String())
	}

	runID := w.Header().Get("X-Run-Id")
	if runID == "" {
		t.Fatal("no X-Run-Id in response")
	}

	res := waitForTerminal(t, env.engine, runID, 30*time.Second)
	if res.Status != registry.StatusSuccess {
		t.Fatalf("pipeline status = %q (reason=%q), want success", res.Status, res.FailureReason)
	}
	parent, err := env.engine.WaitRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("WaitRun: %v", err)
	}
	if parent.ReturnValue != "main" {
		t.Errorf("pipeline return = %v, want \"main\" (GET query param not seeded to stage 0 via ${input.output.branch})", parent.ReturnValue)
	}
}

// TestPipelineWebhookStage0NestedJSON (FIX F) asserts that a webhook POST with
// a nested JSON body resolves ${input.output.<field>} on stage 0 to the nested
// field value (the JSON body is decoded to map[string]interface{} and set as
// Input, so the top-level fields are accessible via ${input.output.<field>}).
func TestPipelineWebhookStage0NestedJSON(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	env := newTestEnv(t)
	dir := t.TempDir()

	stageNested := writeTask(t, dir, "nested-stage",
		`export default async function main({ params }) { return await params.get("action") }`,
		task.TriggerConfig{Manual: true})
	if err := env.reg.Register(stageNested); err != nil {
		t.Fatalf("reg.Register %s: %v", stageNested.ID, err)
	}
	if err := env.engine.Register(stageNested); err != nil {
		t.Fatalf("eng.Register %s: %v", stageNested.ID, err)
	}

	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "nested-pipe", Name: "NP", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Webhook: "/hooks/nested-pipe"},
		Stages: []task.Stage{
			{Task: "nested-stage", Overrides: &task.Overrides{
				Params: task.ParamOverrides{
					// Nested JSON body: {"repo":{"action":"opened"}} — access
					// top-level field "action" directly (one level of nesting
					// is the schema; ${input.output.action} resolves the field
					// named "action" in the top-level JSON object).
					{Name: "action", Default: "${input.output.action}"},
				},
			}},
		},
	}
	if err := env.reg.Register(pipe); err != nil {
		t.Fatalf("reg.Register pipe: %v", err)
	}
	if err := env.engine.Register(pipe); err != nil {
		t.Fatalf("eng.Register pipe: %v", err)
	}

	handler := env.engine.WebhookHandler()
	// POST a nested JSON body: top-level "action" field with a string value.
	body := []byte(`{"action":"opened","repo":{"name":"dicode","stars":42}}`)
	req := httptest.NewRequest("POST", "/hooks/nested-pipe", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("webhook POST status = %d, want 200: %s", w.Code, w.Body.String())
	}

	runID := w.Header().Get("X-Run-Id")
	if runID == "" {
		t.Fatal("no X-Run-Id in response")
	}

	res := waitForTerminal(t, env.engine, runID, 30*time.Second)
	if res.Status != registry.StatusSuccess {
		t.Fatalf("pipeline status = %q (reason=%q), want success", res.Status, res.FailureReason)
	}
	parent, err := env.engine.WaitRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("WaitRun: %v", err)
	}
	if parent.ReturnValue != "opened" {
		t.Errorf("pipeline return = %v, want \"opened\" (nested JSON field not resolved via ${input.output.action})", parent.ReturnValue)
	}
}

// TestPipelineChainNoParamsThreadsUpstreamOutput (FIX B) asserts that a
// chain-triggered pipeline with NO chain.params threads the upstream task's raw
// return value into stage 0 via ${input.output} / ${input.output.<field>},
// mirroring the kind: Task buildChainInput zero-params branch (#350).
func TestPipelineChainNoParamsThreadsUpstreamOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	env := newTestEnv(t)
	dir := t.TempDir()

	// upstream: returns a structured value with a "result" field.
	upstream := writeTask(t, dir, "noparams-up",
		`export default async function main() { return { result: "from-upstream", code: 42 } }`,
		task.TriggerConfig{Manual: true})
	stageNoParams := writeTask(t, dir, "noparams-stage",
		`export default async function main({ params }) { return await params.get("val") }`,
		task.TriggerConfig{Manual: true})
	for _, s := range []*task.Spec{upstream, stageNoParams} {
		if err := env.reg.Register(s); err != nil {
			t.Fatalf("reg.Register %s: %v", s.ID, err)
		}
		if err := env.engine.Register(s); err != nil {
			t.Fatalf("eng.Register %s: %v", s.ID, err)
		}
	}

	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "noparams-pipe", Name: "NoPP", Subtype: "sequential", Enabled: true,
		// chain with NO params — the upstream output must be threaded directly.
		Trigger: task.PipelineTrigger{Chain: &task.ChainTrigger{From: "noparams-up", On: "success"}},
		Stages: []task.Stage{
			{Task: "noparams-stage", Overrides: &task.Overrides{
				Params: task.ParamOverrides{
					// ${input.output.result} resolves to the upstream's "result" field.
					{Name: "val", Default: "${input.output.result}"},
				},
			}},
		},
	}
	if err := env.reg.Register(pipe); err != nil {
		t.Fatalf("reg.Register pipe: %v", err)
	}
	if err := env.engine.Register(pipe); err != nil {
		t.Fatalf("eng.Register pipe: %v", err)
	}

	if _, err := env.engine.FireManual(context.Background(), "noparams-up", nil); err != nil {
		t.Fatalf("FireManual noparams-up: %v", err)
	}

	run := findRun(t, env, "noparams-pipe", registry.RunKindPipeline, 30*time.Second)
	if run.Status != registry.StatusSuccess {
		t.Fatalf("chained pipeline status = %q (reason=%q), want success", run.Status, run.FailureReason)
	}
	parent, err := env.engine.WaitRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("WaitRun: %v", err)
	}
	// Stage 0 must have resolved ${input.output.result} = "from-upstream"
	if parent.ReturnValue != "from-upstream" {
		t.Errorf("pipeline return = %v, want \"from-upstream\" (chain with no params: upstream output not threaded to stage 0 via ${input.output.result})", parent.ReturnValue)
	}
	// Confirm the pipeline was triggered by the chain.
	if run.TriggerSource != registry.TriggerChain {
		t.Errorf("trigger_source = %q, want %q", run.TriggerSource, registry.TriggerChain)
	}
}
