package trigger

// Tests for issue #312: one-shot tasks (manual / cron / webhook / chain)
// may declare trigger.before preflight pipelines. The engine runs the
// pipeline from fireAsync (parallel to startDaemonInternal's invocation
// for daemons); failures surface as a normal run with fail_reason
// "preflight_failed: stage N (<task>)".
//
// Also covers the cycle-detection rule added to validateBeforeRefs:
// once one-shots can declare trigger.before, A→B→A is realisable and
// must be rejected at registration time.

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
)

// TestRegister_RejectsBeforeCycle_TwoNode pins the simplest cycle the
// post-#312 graph admits: A.before=[B], B.before=[A]. Pre-#312 this was
// unrepresentable because trigger.before was daemon-only and entries had
// to be one-shots — Spec.validate would have rejected B.before=[A]
// outright. Now that one-shots can declare trigger.before, validateBefore-
// Refs must catch the cycle.
func TestRegister_RejectsBeforeCycle_TwoNode(t *testing.T) {
	e := newTestEnv(t)

	// Seed A as a one-shot manual task with no before-list.
	a := &task.Spec{
		ID:      "a",
		Name:    "a",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true},
		Enabled: true,
	}
	if err := e.reg.Register(a); err != nil {
		t.Fatalf("seed reg.Register a: %v", err)
	}
	if err := e.engine.Register(a); err != nil {
		t.Fatalf("eng.Register a: %v", err)
	}

	// B references A as a preflight.
	b := &task.Spec{
		ID:      "b",
		Name:    "b",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true, Before: []task.BeforeEntry{{Task: "a"}}},
		Enabled: true,
	}
	if err := e.reg.Register(b); err != nil {
		t.Fatalf("seed reg.Register b: %v", err)
	}
	if err := e.engine.Register(b); err != nil {
		t.Fatalf("eng.Register b: %v", err)
	}

	// Now mutate A to also reference B — this closes the loop A→B→A.
	aWithBefore := &task.Spec{
		ID:      "a",
		Name:    "a",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true, Before: []task.BeforeEntry{{Task: "b"}}},
		Enabled: true,
	}
	// reg.Register replaces the previous A. eng.Register should reject.
	if err := e.reg.Register(aWithBefore); err != nil {
		t.Fatalf("reg.Register aWithBefore: %v", err)
	}
	err := e.engine.Register(aWithBefore)
	if err == nil {
		t.Fatal("expected cycle-detection error, got nil")
	}
	if !strings.Contains(err.Error(), "cycle detected") {
		t.Errorf("expected error to mention cycle, got: %v", err)
	}
}

// TestRegister_RejectsBeforeCycle_ThreeNode exercises a longer cycle
// A→B→C→A so the DFS must walk through an intermediate node before
// observing the back-edge.
func TestRegister_RejectsBeforeCycle_ThreeNode(t *testing.T) {
	e := newTestEnv(t)

	// Seed all three as plain one-shots first; then close the cycle by
	// adding before-edges in a second pass.
	for _, id := range []string{"a", "b", "c"} {
		s := &task.Spec{
			ID:      id,
			Name:    id,
			Runtime: task.RuntimeDeno,
			Trigger: task.TriggerConfig{Manual: true},
			Enabled: true,
		}
		if err := e.reg.Register(s); err != nil {
			t.Fatalf("seed reg.Register %s: %v", id, err)
		}
		if err := e.engine.Register(s); err != nil {
			t.Fatalf("eng.Register %s: %v", id, err)
		}
	}

	// B.before=[A], C.before=[B] — still acyclic.
	bWithBefore := &task.Spec{
		ID:      "b",
		Name:    "b",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true, Before: []task.BeforeEntry{{Task: "a"}}},
		Enabled: true,
	}
	if err := e.reg.Register(bWithBefore); err != nil {
		t.Fatalf("reg.Register bWithBefore: %v", err)
	}
	if err := e.engine.Register(bWithBefore); err != nil {
		t.Fatalf("eng.Register bWithBefore (acyclic): %v", err)
	}

	cWithBefore := &task.Spec{
		ID:      "c",
		Name:    "c",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true, Before: []task.BeforeEntry{{Task: "b"}}},
		Enabled: true,
	}
	if err := e.reg.Register(cWithBefore); err != nil {
		t.Fatalf("reg.Register cWithBefore: %v", err)
	}
	if err := e.engine.Register(cWithBefore); err != nil {
		t.Fatalf("eng.Register cWithBefore (acyclic): %v", err)
	}

	// Now close the cycle: A.before=[C] ⇒ A→C→B→A or, traversed forward,
	// A→C... wait the edges are: A.before=[C] means A depends on C, so
	// the dep edge from A points to C. B.before=[A] → B→A. C.before=[B]
	// → C→B. Cycle: A→C→B→A.
	aClose := &task.Spec{
		ID:      "a",
		Name:    "a",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true, Before: []task.BeforeEntry{{Task: "c"}}},
		Enabled: true,
	}
	if err := e.reg.Register(aClose); err != nil {
		t.Fatalf("reg.Register aClose: %v", err)
	}
	err := e.engine.Register(aClose)
	if err == nil {
		t.Fatal("expected three-node cycle to be rejected, got nil error")
	}
	if !strings.Contains(err.Error(), "cycle detected") {
		t.Errorf("expected cycle-detection error, got: %v", err)
	}
}

// TestRegister_AcceptsBeforeAcyclic confirms the cycle detector doesn't
// false-positive on a diamond / fan-in shape: A.before=[X], B.before=[X]
// (X used by multiple consumers but with no back-edge). This is the
// canonical safe shape — share a preflight stage between several
// downstream tasks.
func TestRegister_AcceptsBeforeAcyclic(t *testing.T) {
	e := newTestEnv(t)

	x := &task.Spec{
		ID:      "x",
		Name:    "x",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true},
		Enabled: true,
	}
	if err := e.reg.Register(x); err != nil {
		t.Fatalf("seed reg.Register x: %v", err)
	}
	if err := e.engine.Register(x); err != nil {
		t.Fatalf("eng.Register x: %v", err)
	}

	a := &task.Spec{
		ID:      "a",
		Name:    "a",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true, Before: []task.BeforeEntry{{Task: "x"}}},
		Enabled: true,
	}
	if err := e.reg.Register(a); err != nil {
		t.Fatalf("reg.Register a: %v", err)
	}
	if err := e.engine.Register(a); err != nil {
		t.Errorf("eng.Register a (acyclic fan-in): unexpected error: %v", err)
	}

	b := &task.Spec{
		ID:      "b",
		Name:    "b",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true, Before: []task.BeforeEntry{{Task: "x"}}},
		Enabled: true,
	}
	if err := e.reg.Register(b); err != nil {
		t.Fatalf("reg.Register b: %v", err)
	}
	if err := e.engine.Register(b); err != nil {
		t.Errorf("eng.Register b (acyclic fan-in): unexpected error: %v", err)
	}
}

// TestRegister_ConcurrentRegisterCannotAdmitCycle pins that the registerMu
// serialization in Register makes the cycle-detection scan atomic with
// respect to other in-flight registrations. Each goroutine mirrors the
// production reconciler pattern (reg.Register then engine.Register) for
// one half of an A→B→A cycle; the engine must never admit both edges.
//
// Without registerMu, two parallel engine.Register calls can interleave
// such that each detectBeforeCycle reads a stale registry snapshot and
// overlays its own candidate edge — passing cycle detection independently
// even though the resulting registry state is cyclic.
//
// We assert the strong invariant: AT LEAST one engine.Register call
// rejects with "cycle detected" — i.e. the final engine state is never
// "both edges admitted". Either both interleavings observe the cycle
// (one race ordering: both reg.Register commit before either engine
// scan) or exactly one observes it (the other ordering: one engine
// scan completes before the second reg.Register commits). Both
// outcomes are correct; the failure mode the mutex eliminates is "zero
// rejections" — a cycle admitted into the engine.
//
// In practice unreachable today because the reconciler is single-threaded,
// but worth pinning for future concurrent-registration paths. Run with
// `-race -count=20` for stress coverage.
func TestRegister_ConcurrentRegisterCannotAdmitCycle(t *testing.T) {
	e := newTestEnv(t)

	// Seed both A and B as plain one-shots so they're known to the registry
	// when the cycle-closing Register calls fire.
	for _, id := range []string{"a", "b"} {
		s := &task.Spec{
			ID:      id,
			Name:    id,
			Runtime: task.RuntimeDeno,
			Trigger: task.TriggerConfig{Manual: true},
			Enabled: true,
		}
		if err := e.reg.Register(s); err != nil {
			t.Fatalf("seed reg.Register %s: %v", id, err)
		}
		if err := e.engine.Register(s); err != nil {
			t.Fatalf("seed eng.Register %s: %v", id, err)
		}
	}

	aWithBefore := &task.Spec{
		ID:      "a",
		Name:    "a",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true, Before: []task.BeforeEntry{{Task: "b"}}},
		Enabled: true,
	}
	bWithBefore := &task.Spec{
		ID:      "b",
		Name:    "b",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true, Before: []task.BeforeEntry{{Task: "a"}}},
		Enabled: true,
	}

	// Mirror production order: reg.Register persists the candidate spec, then
	// engine.Register validates + schedules. Run both pairs in parallel so
	// registerMu's serialization is exercised against arbitrary interleavings
	// of the reg.Register commits.
	start := make(chan struct{})
	var wg sync.WaitGroup
	var errA, errB error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		if err := e.reg.Register(aWithBefore); err != nil {
			errA = err
			return
		}
		errA = e.engine.Register(aWithBefore)
	}()
	go func() {
		defer wg.Done()
		<-start
		if err := e.reg.Register(bWithBefore); err != nil {
			errB = err
			return
		}
		errB = e.engine.Register(bWithBefore)
	}()
	close(start)
	wg.Wait()

	// Invariant: the engine must never admit both edges (i.e. both calls
	// returning nil). At least one — possibly both — must surface the
	// cycle, depending on the order in which the two reg.Register commits
	// land relative to each engine.Register's cycle scan.
	successCount := 0
	cycleCount := 0
	for _, err := range []error{errA, errB} {
		switch {
		case err == nil:
			successCount++
		case strings.Contains(err.Error(), "cycle detected"):
			cycleCount++
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if cycleCount == 0 {
		t.Fatalf("expected at least one cycle-detection error, got success=%d cycle=%d (errA=%v errB=%v)",
			successCount, cycleCount, errA, errB)
	}
	if successCount+cycleCount != 2 {
		t.Fatalf("expected success+cycle to cover both calls, got success=%d cycle=%d (errA=%v errB=%v)",
			successCount, cycleCount, errA, errB)
	}
}

// TestFireManual_PreflightSuccess_FiresBody exercises the happy path: a
// manual task with a 1-stage trigger.before runs the preflight, then
// runs the body. Both end as success runs.
func TestFireManual_PreflightSuccess_FiresBody(t *testing.T) {
	eng, reg, _ := newPreflightEnv(t)

	prereq := makeOneShotSpec("p1")
	if err := reg.Register(prereq); err != nil {
		t.Fatalf("reg.Register p1: %v", err)
	}
	if err := eng.Register(prereq); err != nil {
		t.Fatalf("eng.Register p1: %v", err)
	}

	manual := &task.Spec{
		ID:      "consumer",
		Name:    "consumer",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true, Before: []task.BeforeEntry{{Task: "p1"}}},
		Enabled: true,
	}
	if err := reg.Register(manual); err != nil {
		t.Fatalf("reg.Register consumer: %v", err)
	}
	if err := eng.Register(manual); err != nil {
		t.Fatalf("eng.Register consumer: %v", err)
	}

	runID, err := eng.FireManual(context.Background(), "consumer", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}

	// Wait for the body run to finish successfully.
	waitUntil(t, 5*time.Second, func() bool {
		return hasRunWithStatus(t, reg, "consumer", registry.StatusSuccess)
	}, "consumer body never reached success")

	// The preflight run must exist as a child of the manual run, tagged
	// TriggerPreflight. Verify the per-stage child run was recorded.
	preflightRuns, _ := reg.ListRuns(context.Background(), "p1", 5)
	var found bool
	for _, r := range preflightRuns {
		if r.TriggerSource == registry.TriggerPreflight && r.Status == registry.StatusSuccess {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected at least one successful preflight run for p1")
	}

	// Parent body run should be success — fetch by ID and check status.
	parent, err := reg.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun parent: %v", err)
	}
	if parent.Status != registry.StatusSuccess {
		t.Errorf("parent run status = %q, want success", parent.Status)
	}
	if parent.FailureReason != "" {
		t.Errorf("parent run fail_reason = %q, want empty on success path", parent.FailureReason)
	}
}

// TestFireManual_PreflightFailure_BodyNotFired pins the failure-semantics
// contract: when the preflight stage fails, the parent run surfaces as
// status=failure with fail_reason="preflight_failed: stage N (<task>): …"
// and the body is NEVER dispatched.
func TestFireManual_PreflightFailure_BodyNotFired(t *testing.T) {
	eng, reg, exec := newPreflightEnv(t)

	prereq := makeOneShotSpec("p-fail")
	if err := reg.Register(prereq); err != nil {
		t.Fatalf("reg.Register p-fail: %v", err)
	}
	if err := eng.Register(prereq); err != nil {
		t.Fatalf("eng.Register p-fail: %v", err)
	}
	exec.markFailing("p-fail")

	// Pin a runner-counter on the consumer body so we can prove it never
	// executed — if the preflight short-circuit broke, this count would
	// be > 0.
	var bodyRuns atomic.Int32
	exec.setFn("consumer", func(_, _ string) string {
		bodyRuns.Add(1)
		return registry.StatusSuccess
	})

	consumer := &task.Spec{
		ID:      "consumer",
		Name:    "consumer",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true, Before: []task.BeforeEntry{{Task: "p-fail"}}},
		Enabled: true,
	}
	if err := reg.Register(consumer); err != nil {
		t.Fatalf("reg.Register consumer: %v", err)
	}
	if err := eng.Register(consumer); err != nil {
		t.Fatalf("eng.Register consumer: %v", err)
	}

	runID, err := eng.FireManual(context.Background(), "consumer", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}

	// Parent run must end as failure with a preflight_failed reason.
	waitUntil(t, 5*time.Second, func() bool {
		parent, err := reg.GetRun(context.Background(), runID)
		if err != nil {
			return false
		}
		return parent.Status == registry.StatusFailure
	}, "parent consumer run never recorded as failure")

	parent, err := reg.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun parent: %v", err)
	}
	if !strings.HasPrefix(parent.FailureReason, "preflight_failed:") {
		t.Errorf("parent fail_reason = %q, want prefix \"preflight_failed:\"", parent.FailureReason)
	}
	if !strings.Contains(parent.FailureReason, "p-fail") {
		t.Errorf("parent fail_reason should name the failing stage id; got %q", parent.FailureReason)
	}

	// Body must NEVER have run.
	if got := bodyRuns.Load(); got != 0 {
		t.Errorf("consumer body executed %d times despite preflight failure; want 0", got)
	}
}

// TestFireManual_PreflightChildRunsLinkToParent verifies that each
// preflight stage's run row has parent_run_id set to the one-shot's
// parent fire. The WebUI relies on this linkage to group the preflight
// pipeline under the parent run; without it the children would show up
// as orphans. Daemons can't carry this link (the daemon body run is
// created AFTER preflight clears) — see dispatchPipelineStage's
// parentRunID parameter doc.
func TestFireManual_PreflightChildRunsLinkToParent(t *testing.T) {
	eng, reg, _ := newPreflightEnv(t)

	prereq := makeOneShotSpec("link-prereq")
	if err := reg.Register(prereq); err != nil {
		t.Fatalf("reg.Register prereq: %v", err)
	}
	if err := eng.Register(prereq); err != nil {
		t.Fatalf("eng.Register prereq: %v", err)
	}

	consumer := &task.Spec{
		ID:      "link-consumer",
		Name:    "link-consumer",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true, Before: []task.BeforeEntry{{Task: "link-prereq"}}},
		Enabled: true,
	}
	if err := reg.Register(consumer); err != nil {
		t.Fatalf("reg.Register consumer: %v", err)
	}
	if err := eng.Register(consumer); err != nil {
		t.Fatalf("eng.Register consumer: %v", err)
	}

	parentRunID, err := eng.FireManual(context.Background(), "link-consumer", nil)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}

	waitUntil(t, 5*time.Second, func() bool {
		return hasRunWithStatus(t, reg, "link-consumer", registry.StatusSuccess)
	}, "consumer body never succeeded")

	// Children listed via parent_run_id must include the preflight
	// stage row for link-prereq.
	children, err := reg.ListChildren(context.Background(), parentRunID, 10)
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	var foundPreflight bool
	for _, c := range children {
		if c.TaskID == "link-prereq" && c.TriggerSource == registry.TriggerPreflight {
			foundPreflight = true
		}
	}
	if !foundPreflight {
		t.Errorf("expected preflight child run linked to parent %s; got children=%+v",
			parentRunID, children)
	}
}

// TestFireManual_PipesInputOutputThroughStages mirrors
// TestBefore_PipesInputOutputThroughStages but for a manual one-shot.
// Confirms the ${input.output} substitution flow is identical regardless
// of trigger type — the only thing that changed in #312 is *which*
// trigger kinds may declare a before-list.
func TestFireManual_PipesInputOutputThroughStages(t *testing.T) {
	eng, reg, exec := newPreflightEnv(t)

	exec.setReturnFn("render", func(_, _ string, _ *task.Spec) interface{} {
		return "manual-pipeline-value"
	})
	render := makeOneShotSpec("render")
	if err := reg.Register(render); err != nil {
		t.Fatalf("register render: %v", err)
	}
	if err := eng.Register(render); err != nil {
		t.Fatalf("eng.Register render: %v", err)
	}

	writer := &task.Spec{
		ID:      "write",
		Name:    "write",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true},
		Params:  task.Params{{Name: "content", Required: true}},
		Enabled: true,
	}
	if err := reg.Register(writer); err != nil {
		t.Fatalf("register writer: %v", err)
	}
	if err := eng.Register(writer); err != nil {
		t.Fatalf("eng.Register writer: %v", err)
	}

	consumer := &task.Spec{
		ID:      "consumer",
		Name:    "consumer",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{
			Manual: true,
			Before: []task.BeforeEntry{
				{Task: "render"},
				{
					Task: "write",
					Overrides: &task.Overrides{
						Params: task.ParamOverrides{
							{Name: "content", Default: "${input.output}"},
						},
					},
				},
			},
		},
		Enabled: true,
	}
	if err := reg.Register(consumer); err != nil {
		t.Fatalf("register consumer: %v", err)
	}
	if err := eng.Register(consumer); err != nil {
		t.Fatalf("eng.Register consumer: %v", err)
	}

	if _, err := eng.FireManual(context.Background(), "consumer", nil); err != nil {
		t.Fatalf("FireManual: %v", err)
	}

	// Wait until the writer stage captured a spec we can inspect.
	var writeSpec *task.Spec
	waitUntil(t, 5*time.Second, func() bool {
		runs, _ := reg.ListRuns(context.Background(), "write", 5)
		for _, r := range runs {
			if r.TriggerSource == registry.TriggerPreflight {
				if s := exec.specForRun(r.ID); s != nil {
					writeSpec = s
					return true
				}
			}
		}
		return false
	}, "write preflight stage never captured")

	var got string
	for _, p := range writeSpec.Params {
		if p.Name == "content" {
			got = p.Default
		}
	}
	if got != "manual-pipeline-value" {
		t.Errorf("write.content default = %q; want %q (upstream output did not pipe through)",
			got, "manual-pipeline-value")
	}

	// Consumer body must reach success — confirms both stages passed and
	// the body did dispatch.
	waitUntil(t, 5*time.Second, func() bool {
		return hasRunWithStatus(t, reg, "consumer", registry.StatusSuccess)
	}, "consumer body never reached success")
}
