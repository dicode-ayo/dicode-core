package trigger

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/audit"
	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// hookRecord is one runStartedHook / runFinishedHook invocation.
type hookRecord struct {
	taskID, runID, status, source string
}

// hookSpy captures every lifecycle hook the engine fires, so a test can
// assert that a kind: PipelineTask parent run is bookkept exactly like a
// kind: Task run.
type hookSpy struct {
	mu       sync.Mutex
	started  []hookRecord
	finished []hookRecord
}

func (h *hookSpy) attach(e *Engine) {
	e.SetRunStartedHook(func(taskID, runID, source string) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.started = append(h.started, hookRecord{taskID: taskID, runID: runID, source: source})
	})
	e.AddRunFinishedHook(func(taskID, runID, status, source string, _ int64) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.finished = append(h.finished, hookRecord{taskID: taskID, runID: runID, status: status, source: source})
	})
}

// awaitFinished blocks until a run:finished hook for runID arrives, and
// returns it together with the matching run:started record.
func (h *hookSpy) awaitFinished(t *testing.T, runID string, timeout time.Duration) (started, finished hookRecord) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		for _, r := range h.started {
			if r.runID == runID {
				started = r
			}
		}
		for _, r := range h.finished {
			if r.runID == runID {
				finished = r
			}
		}
		h.mu.Unlock()
		if finished.runID != "" {
			return started, finished
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no run:finished hook for %q within %v", runID, timeout)
	return started, finished
}

func (h *hookSpy) counts() (int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.started), len(h.finished)
}

// lifecycleFixture registers one kind: Task and one kind: PipelineTask that
// wraps it, so a table case can fire either kind through the same entry point.
type lifecycleFixture struct {
	env      *testEnv
	spy      *hookSpy
	stageID  string
	pipeline string
}

func newLifecycleFixture(t *testing.T) *lifecycleFixture {
	t.Helper()
	env := newTestEnv(t)
	// Audit emission piggybacks on the DB handle; without it e.audit is nil
	// and every Emit is a silent no-op, which would make the audit assertions
	// below vacuous.
	env.engine.SetDB(env.db)

	dir := t.TempDir()
	stage := writeTask(t, dir, "lifecycle-stage",
		`export default async function main() { return "ok" }`, task.TriggerConfig{Manual: true})
	if err := env.reg.Register(stage); err != nil {
		t.Fatal(err)
	}
	if err := env.engine.Register(stage); err != nil {
		t.Fatal(err)
	}
	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "lifecycle-pipe", Name: "Lifecycle Pipe", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Manual: true},
		Stages:  []task.Stage{{Task: "lifecycle-stage"}},
	}
	if err := env.reg.Register(pipe); err != nil {
		t.Fatal(err)
	}
	if err := env.engine.Register(pipe); err != nil {
		t.Fatal(err)
	}
	spy := &hookSpy{}
	spy.attach(env.engine)
	return &lifecycleFixture{env: env, spy: spy, stageID: stage.ID, pipeline: pipe.ID}
}

// register puts a spec in both the registry and the engine.
func (f *lifecycleFixture) register(t *testing.T, s *task.Spec) {
	t.Helper()
	if err := f.env.reg.Register(s); err != nil {
		t.Fatal(err)
	}
	if err := f.env.engine.Register(s); err != nil {
		t.Fatal(err)
	}
}

// awaitRunUntil reports whether any run row for taskID appears within timeout.
func (f *lifecycleFixture) awaitRunUntil(taskID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if runs, _ := f.env.reg.ListRuns(context.Background(), taskID, 1); len(runs) > 0 {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// fire fires the named task or pipeline through fireKinded — the one entry
// point both kinds share.
func (f *lifecycleFixture) fire(t *testing.T, id string, opts pkgruntime.RunOptions) (string, error) {
	t.Helper()
	k, ok := f.env.reg.GetKinded(id)
	if !ok {
		t.Fatalf("registry has no %q", id)
	}
	return f.env.engine.fireKinded(context.Background(), k, opts, registry.TriggerManual)
}

// fireDirect bypasses fireKinded and calls the kind's own fire function.
func (f *lifecycleFixture) fireDirect(t *testing.T, id string) (string, error) {
	t.Helper()
	k, ok := f.env.reg.GetKinded(id)
	if !ok {
		t.Fatalf("registry has no %q", id)
	}
	if p, isPipe := k.(*task.PipelineTask); isPipe {
		return f.env.engine.firePipeline(context.Background(), p, pkgruntime.RunOptions{}, registry.TriggerManual)
	}
	return f.env.engine.fireAsync(context.Background(), k.(*task.Spec), pkgruntime.RunOptions{}, registry.TriggerManual)
}

func (f *lifecycleFixture) auditRunTriggered(t *testing.T, targetID, runID string) *audit.Event {
	t.Helper()
	evs, err := f.env.engine.audit.Query(context.Background(), audit.Filter{
		TaskID: targetID, EventType: audit.EventRunTriggered, Limit: 50,
	})
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	for i := range evs {
		if evs[i].RunID == runID {
			return &evs[i]
		}
	}
	return nil
}

// TestRunLifecycleBookkeeping asserts that every cell of
// {Task, Pipeline} × {guarded, unguarded} agrees on what a Run is: a fire guard
// that refuses without a trace, a run_triggered audit row naming the kind, and a
// matched run:started / run:finished hook pair.
func TestRunLifecycleBookkeeping(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	cases := []struct {
		name       string
		pipeline   bool
		guarded    bool
		wantKind   string
		wantTarget string
	}{
		{name: "task/unguarded", wantKind: registry.RunKindTask, wantTarget: "task"},
		{name: "task/guarded", guarded: true},
		{name: "pipeline/unguarded", pipeline: true, wantKind: registry.RunKindPipeline, wantTarget: "pipeline"},
		{name: "pipeline/guarded", pipeline: true, guarded: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLifecycleFixture(t)
			id := f.stageID
			if tc.pipeline {
				id = f.pipeline
			}
			if tc.guarded {
				f.env.engine.SetFireGuard(func(taskID string) error {
					if taskID == id {
						return fmt.Errorf("task pending approval: %s", taskID)
					}
					return nil
				})
			}

			runID, err := f.fire(t, id, pkgruntime.RunOptions{})

			if tc.guarded {
				if err == nil {
					t.Fatalf("guarded fire of %q returned run %q; want a veto", id, runID)
				}
				// The guard belongs to the Run, not to fireKinded: a caller
				// that reaches the kind's own fire function directly is vetoed
				// on the same terms.
				if directID, derr := f.fireDirect(t, id); derr == nil {
					t.Errorf("direct fire of %q returned run %q; want the same veto", id, directID)
				}
				// A vetoed fire leaves no trace: no run row, no hooks. It never
				// became a Run.
				runs, _ := f.env.reg.ListRuns(context.Background(), id, 5)
				if len(runs) != 0 {
					t.Errorf("guarded fire created %d run rows; want 0", len(runs))
				}
				if s, fin := f.spy.counts(); s != 0 || fin != 0 {
					t.Errorf("guarded fire fired hooks: started=%d finished=%d; want 0/0", s, fin)
				}
				return
			}

			if err != nil {
				t.Fatalf("fire %q: %v", id, err)
			}
			started, finished := f.spy.awaitFinished(t, runID, 60*time.Second)
			if started.runID != runID {
				t.Errorf("no run:started hook for run %q", runID)
			}
			if started.taskID != id || started.source != string(registry.TriggerManual) {
				t.Errorf("run:started = %+v; want task %q source %q", started, id, registry.TriggerManual)
			}
			if finished.taskID != id || finished.status != registry.StatusSuccess {
				t.Errorf("run:finished = %+v; want task %q status success", finished, id)
			}

			run, err := f.env.reg.GetRun(context.Background(), runID)
			if err != nil {
				t.Fatalf("get run: %v", err)
			}
			if run.Kind != tc.wantKind {
				t.Errorf("run kind = %q; want %q", run.Kind, tc.wantKind)
			}

			ev := f.auditRunTriggered(t, id, runID)
			if ev == nil {
				t.Fatalf("no %s audit row for run %q", audit.EventRunTriggered, runID)
			}
			if ev.TargetKind != tc.wantTarget {
				t.Errorf("audit target_kind = %q; want %q", ev.TargetKind, tc.wantTarget)
			}
			if ev.ActorKind != string(registry.TriggerManual) {
				t.Errorf("audit actor_kind = %q; want %q", ev.ActorKind, registry.TriggerManual)
			}
		})
	}
}

// TestRunLifecycleChainDepthCeiling is the table's third axis: a run of either
// kind carries the chain hop count it was fired with, so maxSuccessChainDepth
// refuses a run of either kind at the same hop.
//
// The subject is fired over real chain edges rather than with a seeded
// _chain_depth: a bare edge (no declared trigger.chain.params) hands the
// downstream the upstream's raw return value, with no envelope to carry a hop
// count, so seeding the payload would assert a shape no chain edge produces.
//
// Whether the SUBJECT runs is the assertion — not whether something downstream
// of it does. A subject at the ceiling with a task downstream would be refused
// by the task-side check in fireSuccessChains whichever kind the subject was,
// leaving firePipelineChains' own check unexercised.
func TestRunLifecycleChainDepthCeiling(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	cases := []struct {
		name     string
		pipeline bool
		// depth is the hop the subject sits at, counting the manually-fired
		// root as 0. It is reached by chaining depth-1 bare relay edges.
		depth    int
		wantFire bool
	}{
		{name: "task/below-ceiling", depth: 1, wantFire: true},
		{name: "task/past-ceiling", depth: maxSuccessChainDepth + 1, wantFire: false},
		{name: "pipeline/below-ceiling", pipeline: true, depth: 1, wantFire: true},
		{name: "pipeline/past-ceiling", pipeline: true, depth: maxSuccessChainDepth + 1, wantFire: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLifecycleFixture(t)
			dir := t.TempDir()

			// root → relay-1 → … → relay-(depth-1) → subject, every edge bare.
			// Registered leaf-last so no edge is ever proposed into a graph that
			// already closes a loop.
			root := writeTask(t, dir, "depth-root",
				`export default async function main() { return "r" }`, task.TriggerConfig{Manual: true})
			f.register(t, root)
			upstream := root.ID
			for i := 1; i < tc.depth; i++ {
				id := fmt.Sprintf("depth-relay-%d", i)
				relay := writeTask(t, dir, id, `export default async function main() { return "x" }`,
					task.TriggerConfig{Chain: &task.ChainTrigger{From: upstream, On: registry.StatusSuccess}})
				f.register(t, relay)
				upstream = id
			}

			var subjectID string
			if tc.pipeline {
				pipe := &task.PipelineTask{
					APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
					ID: "depth-pipe", Name: "Depth Pipe", Subtype: "sequential", Enabled: true,
					Trigger: task.PipelineTrigger{Chain: &task.ChainTrigger{From: upstream, On: registry.StatusSuccess}},
					Stages:  []task.Stage{{Task: f.stageID}},
				}
				if err := f.env.reg.Register(pipe); err != nil {
					t.Fatal(err)
				}
				if err := f.env.engine.Register(pipe); err != nil {
					t.Fatal(err)
				}
				subjectID = pipe.ID
			} else {
				subject := writeTask(t, dir, "depth-subject",
					`export default async function main() { return "s" }`,
					task.TriggerConfig{Chain: &task.ChainTrigger{From: upstream, On: registry.StatusSuccess}})
				f.register(t, subject)
				subjectID = subject.ID
			}

			rootRun, err := f.fire(t, root.ID, pkgruntime.RunOptions{})
			if err != nil {
				t.Fatalf("fire root: %v", err)
			}
			f.spy.awaitFinished(t, rootRun, 60*time.Second)

			// The chain dispatches asynchronously; the subject's absence is only
			// meaningful once the window elapses, so the refused case pays the
			// full wait.
			fired := f.awaitRunUntil(subjectID, 20*time.Second)
			if fired != tc.wantFire {
				t.Errorf("subject %q at depth %d fired = %v; want %v", subjectID, tc.depth, fired, tc.wantFire)
			}
		})
	}
}

// TestSuccessChainCycleSpansKinds asserts the registration-time cycle guard
// sees trigger.chain edges declared by either kind, in both directions: the
// closing edge may be the task's or the pipeline's.
func TestSuccessChainCycleSpansKinds(t *testing.T) {
	env := newTestEnv(t)

	a := &task.Spec{ID: "cycle-a", Name: "A", Enabled: true, Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true}}
	if err := env.reg.Register(a); err != nil {
		t.Fatal(err)
	}
	if err := env.engine.Register(a); err != nil {
		t.Fatal(err)
	}

	// P fires when A succeeds.
	p := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "cycle-p", Name: "P", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Chain: &task.ChainTrigger{From: "cycle-a", On: registry.StatusSuccess}},
		Stages:  []task.Stage{{Task: "cycle-a"}},
	}
	if err := env.reg.Register(p); err != nil {
		t.Fatal(err)
	}
	if err := env.engine.Register(p); err != nil {
		t.Fatalf("register pipeline: %v", err)
	}

	// B fires when P succeeds and A fires when B succeeds — closing A → P → B → A.
	b := &task.Spec{ID: "cycle-b", Name: "B", Enabled: true, Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Chain: &task.ChainTrigger{From: "cycle-p", On: registry.StatusSuccess}}}
	if err := env.reg.Register(b); err != nil {
		t.Fatal(err)
	}
	if err := env.engine.Register(b); err != nil {
		t.Fatal(err)
	}

	a2 := &task.Spec{ID: "cycle-a", Name: "A", Enabled: true, Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Chain: &task.ChainTrigger{From: "cycle-b", On: registry.StatusSuccess}}}
	if err := env.engine.Register(a2); err == nil {
		t.Fatal("registering cycle-a chained from cycle-b was accepted; the cycle runs A → P → B → A")
	}

	// The same guard applies when the closing edge is the pipeline's: a
	// pipeline chained from a task that is itself chained from the pipeline.
	c := &task.Spec{ID: "cycle-c", Name: "C", Enabled: true, Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Chain: &task.ChainTrigger{From: "cycle-q", On: registry.StatusSuccess}}}
	if err := env.reg.Register(c); err != nil {
		t.Fatal(err)
	}
	if err := env.engine.Register(c); err != nil {
		t.Fatal(err)
	}
	q := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "cycle-q", Name: "Q", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Chain: &task.ChainTrigger{From: "cycle-c", On: registry.StatusSuccess}},
		Stages:  []task.Stage{{Task: "cycle-c"}},
	}
	if err := env.reg.Register(q); err != nil {
		t.Fatal(err)
	}
	if err := env.engine.Register(q); err == nil {
		t.Fatal("registering pipeline cycle-q chained from cycle-c was accepted; the cycle runs C → Q → C")
	}
}

// TestChainDepthIgnoresCallerSuppliedEnvelope pins where a run's hop count may
// come from. Every chain dispatch sets RunOptions.ChainDepth; a replay restores
// the depth from the payload it re-fires, because that is the only record left
// of where the run sat. No other source may, and a webhook fire in particular
// must not: opts.Input is the request body there, so honoring a _chain_depth
// inside it would let a caller choose the ceiling bounding its own run's chain.
func TestChainDepthIgnoresCallerSuppliedEnvelope(t *testing.T) {
	envelope := map[string]any{"_chain_depth": 7}
	cases := []struct {
		name   string
		opts   pkgruntime.RunOptions
		source registry.TriggerSource
		want   int
	}{
		{name: "webhook body is ignored", opts: pkgruntime.RunOptions{Input: envelope},
			source: registry.TriggerWebhook, want: 0},
		{name: "manual input is ignored", opts: pkgruntime.RunOptions{Input: envelope},
			source: registry.TriggerManual, want: 0},
		{name: "replay restores the persisted depth", opts: pkgruntime.RunOptions{Input: envelope},
			source: registry.TriggerReplay, want: 7},
		{name: "chain dispatch wins over the envelope",
			opts:   pkgruntime.RunOptions{Input: map[string]any{"_chain_depth": 99}, ChainDepth: 3},
			source: registry.TriggerChain, want: 3},
		{name: "negative replay depth is no hop count",
			opts:   pkgruntime.RunOptions{Input: map[string]any{"_chain_depth": -5}},
			source: registry.TriggerReplay, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			got, ok := chainDepthOf(&opts, tc.source)
			if !ok {
				got = 0
			}
			if got != tc.want {
				t.Errorf("chainDepthOf(%v) = %d; want %d", tc.source, got, tc.want)
			}
		})
	}
}

// TestCronCatchupFiresMissedPipeline pins the whole catchup path over kind:
// PipelineTask. scheduleCron persists a cron_jobs row for a pipeline the same
// way it does for a task, so both halves of catchupMissedCronRuns have to agree
// on which kinds own a row: the orphan prune must keep it, and the fire must
// resolve it. A prune over the kind: Task inventory alone deletes the row as
// orphaned before the query that reads it, which no assertion on the fire half
// alone would catch.
func TestCronCatchupFiresMissedPipeline(t *testing.T) {
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)

	dir := t.TempDir()
	stage := writeTask(t, dir, "catchup-stage", `return 1`, task.TriggerConfig{Manual: true})
	if err := reg.Register(stage); err != nil {
		t.Fatal(err)
	}
	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "catchup-pipe", Name: "Catchup Pipe", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Cron: "0 9 * * *"},
		Stages:  []task.Stage{{Task: "catchup-stage"}},
	}
	if err := reg.Register(pipe); err != nil {
		t.Fatal(err)
	}

	// A stale row from the previous session, as scheduleCron would have left it.
	missedAt := time.Now().Add(-5 * time.Minute).Unix()
	if err := d.Exec(context.Background(),
		`INSERT INTO cron_jobs(task_id,cron_expr,next_run_at) VALUES(?,?,?)`,
		"catchup-pipe", "0 9 * * *", missedAt,
	); err != nil {
		t.Fatalf("seed cron_jobs: %v", err)
	}

	eng := New(reg, nil, zap.NewNop())
	eng.SetDB(d)
	t.Cleanup(func() { reapEngineRuns(eng, 10*time.Second) })

	eng.catchupMissedCronRuns(context.Background())

	// The row must survive the orphan prune…
	var surviving int
	if err := d.Query(context.Background(),
		`SELECT COUNT(*) FROM cron_jobs WHERE task_id=?`, []any{"catchup-pipe"},
		func(rows db.Scanner) error {
			if rows.Next() {
				return rows.Scan(&surviving)
			}
			return nil
		},
	); err != nil {
		t.Fatalf("count cron_jobs: %v", err)
	}
	if surviving != 1 {
		t.Errorf("pipeline cron_jobs row pruned as orphaned: %d rows remain, want 1", surviving)
	}

	// …and the missed tick must fire.
	runs, err := reg.ListRuns(context.Background(), "catchup-pipe", 5)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	var found bool
	for _, r := range runs {
		if r.TriggerSource == registry.TriggerCronCatchup {
			found = true
		}
	}
	if !found {
		t.Errorf("no cron-catchup run for the missed pipeline tick; got %d run(s)", len(runs))
	}
}
