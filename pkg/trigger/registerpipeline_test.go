package trigger

import (
	"testing"

	"github.com/dicode/dicode/pkg/task"
)

// registerStageAndPipeline registers a trivial kind: Task stage plus the given
// pipeline (which references it) through both the registry and the engine.
func registerStageAndPipeline(t *testing.T, env *testEnv, pipe *task.PipelineTask) {
	t.Helper()
	stage := &task.Spec{ID: "s", Name: "S", Enabled: true,
		Runtime: task.RuntimeDeno, Trigger: task.TriggerConfig{Manual: true}}
	if err := env.reg.Register(stage); err != nil {
		t.Fatal(err)
	}
	if err := env.engine.Register(stage); err != nil {
		t.Fatal(err)
	}
	if err := env.reg.Register(pipe); err != nil {
		t.Fatal(err)
	}
	if err := env.engine.Register(pipe); err != nil {
		t.Fatalf("register pipeline: %v", err)
	}
}

// TestPipelineCronRegistered asserts a cron-triggered pipeline gets a cron entry
// scheduled under its ID (so the scheduler will fire it).
func TestPipelineCronRegistered(t *testing.T) {
	env := newTestEnv(t)
	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "p", Name: "P", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Cron: "0 0 * * *"},
		Stages:  []task.Stage{{Task: "s"}},
	}
	registerStageAndPipeline(t, env, pipe)

	env.engine.mu.Lock()
	_, ok := env.engine.cronEntries["p"]
	env.engine.mu.Unlock()
	if !ok {
		t.Fatal("no cron entry registered for cron-triggered pipeline")
	}
}

// TestPipelineWebhookRegistered asserts a webhook-triggered pipeline claims its
// path in the engine's webhook routing table.
func TestPipelineWebhookRegistered(t *testing.T) {
	env := newTestEnv(t)
	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "wp", Name: "WP", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Webhook: "/hooks/pipe"},
		Stages:  []task.Stage{{Task: "s"}},
	}
	registerStageAndPipeline(t, env, pipe)

	env.engine.mu.Lock()
	got := env.engine.webhooks["/hooks/pipe"]
	env.engine.mu.Unlock()
	if got != "wp" {
		t.Fatalf("webhook path not claimed by pipeline: got %q, want wp", got)
	}
}

func TestValidatePipelineRefs(t *testing.T) {
	env := newTestEnv(t)
	stage := &task.Spec{ID: "stage-a", Name: "A", Enabled: true,
		Runtime: task.RuntimeDeno, Trigger: task.TriggerConfig{Manual: true}}
	if err := env.reg.Register(stage); err != nil {
		t.Fatal(err)
	}

	good := &task.PipelineTask{ID: "p", Name: "P", Subtype: "sequential", Enabled: true,
		Stages: []task.Stage{{Task: "stage-a"}}}
	if err := env.engine.validatePipelineRefs(good); err != nil {
		t.Fatalf("good pipeline rejected: %v", err)
	}

	unknown := &task.PipelineTask{ID: "p2", Name: "P2", Subtype: "sequential",
		Stages: []task.Stage{{Task: "nope"}}}
	if err := env.engine.validatePipelineRefs(unknown); err == nil {
		t.Fatal("expected unknown-task error")
	}

	// A pipeline cannot be a stage (v1: stages must be kind: Task).
	if err := env.reg.Register(good); err != nil {
		t.Fatal(err)
	}
	nested := &task.PipelineTask{ID: "p3", Name: "P3", Subtype: "sequential",
		Stages: []task.Stage{{Task: "p"}}}
	if err := env.engine.validatePipelineRefs(nested); err == nil {
		t.Fatal("expected stages-must-be-Task error")
	}
}

func TestDetectPipelineCycle(t *testing.T) {
	env := newTestEnv(t)
	a := &task.Spec{ID: "a", Name: "A", Enabled: true, Runtime: task.RuntimeDeno, Trigger: task.TriggerConfig{Manual: true}}
	_ = env.reg.Register(a)
	// Self-cycle: pipeline whose stage references itself is caught by Validate (self-ref),
	// so test a 2-node cycle via two pipelines referencing each other is impossible in v1
	// (stages must be Task). So just assert a healthy pipeline has no cycle.
	p := &task.PipelineTask{ID: "p", Name: "P", Subtype: "sequential", Enabled: true,
		Stages: []task.Stage{{Task: "a"}}}
	if got := env.engine.detectPipelineCycle(p); got != "" {
		t.Fatalf("unexpected cycle: %q", got)
	}
}

func TestValidatePipelineRefs_InputRefStageAccepted(t *testing.T) {
	env := newTestEnv(t)
	stage := &task.Spec{ID: "writer", Name: "Writer", Enabled: true,
		Runtime: task.RuntimeDeno, Trigger: task.TriggerConfig{Manual: true}}
	if err := env.reg.Register(stage); err != nil {
		t.Fatal(err)
	}
	p := &task.PipelineTask{ID: "p", Name: "P", Subtype: "sequential", Enabled: true,
		Stages: []task.Stage{
			{Task: "writer", Overrides: &task.Overrides{
				Params: task.ParamOverrides{{Name: "content", Default: "${input.output}"}}}},
		},
	}
	if err := env.engine.validatePipelineRefs(p); err != nil {
		t.Fatalf("stage with ${input.output} override should be accepted, got: %v", err)
	}
}

func TestRegisterPipelineValidates(t *testing.T) {
	env := newTestEnv(t)
	stage := &task.Spec{ID: "s", Name: "S", Enabled: true, Runtime: task.RuntimeDeno, Trigger: task.TriggerConfig{Manual: true}}
	_ = env.reg.Register(stage)

	good := &task.PipelineTask{
		APIVersion: "dicode/v1",
		Kind:       task.KindPipelineTask,
		ID:         "p",
		Name:       "P",
		Subtype:    "sequential",
		Enabled:    true,
		Stages:     []task.Stage{{Task: "s"}},
	}
	if err := env.engine.registerPipeline(good); err != nil {
		t.Fatalf("registerPipeline(good) = %v", err)
	}
	// Invalid subtype is rejected (Validate runs inside registerPipeline).
	bad := &task.PipelineTask{
		APIVersion: "dicode/v1",
		Kind:       task.KindPipelineTask,
		ID:         "p2",
		Name:       "P2",
		Subtype:    "parallel",
		Stages:     []task.Stage{{Task: "s"}},
	}
	if err := env.engine.registerPipeline(bad); err == nil {
		t.Fatal("expected subtype rejection")
	}
}

// TestColdStartDeferredPipeline is the regression test for GAP 2 (#341): on a
// fresh daemon the reconciler can register a pipeline BEFORE its stage tasks.
// registerPipeline → validatePipelineRefs then fails ("stage task not found"),
// and nothing retried — so the pipeline's cron/webhook never got scheduled
// until its file changed. The fix records such pipelines in deferredPipelines
// and retries them when a kind: Task is later registered successfully.
func TestColdStartDeferredPipeline(t *testing.T) {
	env := newTestEnv(t)

	// 1. Register the pipeline FIRST — its stage "s" is not yet registered.
	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "p", Name: "P", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Cron: "0 0 * * *"},
		Stages:  []task.Stage{{Task: "s"}},
	}
	if err := env.reg.Register(pipe); err != nil {
		t.Fatalf("registry.Register(pipe): %v", err)
	}
	// Register must NOT return a fatal error — the pipeline is deferred.
	if err := env.engine.Register(pipe); err != nil {
		t.Fatalf("Register(pipe) before stage should defer, not error: %v", err)
	}
	// It must NOT be scheduled yet (no cron entry).
	env.engine.mu.Lock()
	_, scheduled := env.engine.cronEntries["p"]
	env.engine.mu.Unlock()
	if scheduled {
		t.Fatal("pipeline should not be scheduled before its stage exists")
	}
	// It must be recorded as deferred.
	env.engine.registerMu.Lock()
	_, deferred := env.engine.deferredPipelines["p"]
	env.engine.registerMu.Unlock()
	if !deferred {
		t.Fatal("pipeline with missing stage should be recorded as deferred")
	}

	// 2. Now register the stage task — WITHOUT re-submitting the pipeline.
	stage := &task.Spec{ID: "s", Name: "S", Enabled: true,
		Runtime: task.RuntimeDeno, Trigger: task.TriggerConfig{Manual: true}}
	if err := env.reg.Register(stage); err != nil {
		t.Fatal(err)
	}
	if err := env.engine.Register(stage); err != nil {
		t.Fatalf("Register(stage): %v", err)
	}

	// 3. The pipeline must now be scheduled (cron entry appears) and dropped
	// from the deferred set.
	env.engine.mu.Lock()
	_, scheduled = env.engine.cronEntries["p"]
	env.engine.mu.Unlock()
	if !scheduled {
		t.Fatal("pipeline was not scheduled after its stage registered (deferred-retry failed)")
	}
	env.engine.registerMu.Lock()
	_, stillDeferred := env.engine.deferredPipelines["p"]
	env.engine.registerMu.Unlock()
	if stillDeferred {
		t.Fatal("pipeline should be removed from deferred set once it schedules")
	}
}

// TestColdStartDeferredPipeline_GenuinelyMissingStaysUnscheduled confirms a
// pipeline whose stage is never registered stays unscheduled forever — no
// crash, no infinite retry, just a permanently-deferred entry that never
// schedules. Registering an UNRELATED stage triggers a retry attempt that must
// fail cleanly and leave the pipeline deferred.
func TestColdStartDeferredPipeline_GenuinelyMissingStaysUnscheduled(t *testing.T) {
	env := newTestEnv(t)

	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "p", Name: "P", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Cron: "0 0 * * *"},
		Stages:  []task.Stage{{Task: "never-exists"}},
	}
	if err := env.reg.Register(pipe); err != nil {
		t.Fatal(err)
	}
	if err := env.engine.Register(pipe); err != nil {
		t.Fatalf("Register(pipe) should defer, not error: %v", err)
	}

	// Register an unrelated stage — triggers a retry that must fail cleanly.
	other := &task.Spec{ID: "other", Name: "Other", Enabled: true,
		Runtime: task.RuntimeDeno, Trigger: task.TriggerConfig{Manual: true}}
	if err := env.reg.Register(other); err != nil {
		t.Fatal(err)
	}
	if err := env.engine.Register(other); err != nil {
		t.Fatalf("Register(other): %v", err)
	}

	env.engine.mu.Lock()
	_, scheduled := env.engine.cronEntries["p"]
	env.engine.mu.Unlock()
	if scheduled {
		t.Fatal("pipeline with a genuinely-missing stage must never schedule")
	}
	env.engine.registerMu.Lock()
	_, deferred := env.engine.deferredPipelines["p"]
	env.engine.registerMu.Unlock()
	if !deferred {
		t.Fatal("genuinely-missing pipeline should remain deferred")
	}
}

// TestColdStartDeferredPipeline_DroppedOnUnregister confirms a deferred
// pipeline that is later unregistered is removed from the deferred set, so a
// subsequent stage registration does not resurrect a stale pipeline.
func TestColdStartDeferredPipeline_DroppedOnUnregister(t *testing.T) {
	env := newTestEnv(t)

	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "p", Name: "P", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Cron: "0 0 * * *"},
		Stages:  []task.Stage{{Task: "s"}},
	}
	if err := env.reg.Register(pipe); err != nil {
		t.Fatal(err)
	}
	if err := env.engine.Register(pipe); err != nil {
		t.Fatalf("Register(pipe): %v", err)
	}

	// Unregister the deferred pipeline (e.g. its file was removed).
	env.reg.Unregister("p")
	env.engine.Unregister("p")

	env.engine.registerMu.Lock()
	_, deferred := env.engine.deferredPipelines["p"]
	env.engine.registerMu.Unlock()
	if deferred {
		t.Fatal("unregistered pipeline must be dropped from the deferred set")
	}

	// Now register the stage — the dropped pipeline must NOT come back.
	stage := &task.Spec{ID: "s", Name: "S", Enabled: true,
		Runtime: task.RuntimeDeno, Trigger: task.TriggerConfig{Manual: true}}
	if err := env.reg.Register(stage); err != nil {
		t.Fatal(err)
	}
	if err := env.engine.Register(stage); err != nil {
		t.Fatal(err)
	}
	env.engine.mu.Lock()
	_, scheduled := env.engine.cronEntries["p"]
	env.engine.mu.Unlock()
	if scheduled {
		t.Fatal("dropped pipeline must not be resurrected by a later stage registration")
	}
}

func TestEngineRegisterRoutesByKind(t *testing.T) {
	env := newTestEnv(t)
	stage := &task.Spec{ID: "s", Name: "S", Enabled: true,
		Runtime: task.RuntimeDeno, Trigger: task.TriggerConfig{Manual: true}}
	if err := env.reg.Register(stage); err != nil {
		t.Fatal(err)
	}
	if err := env.engine.Register(stage); err != nil { // *task.Spec path still works
		t.Fatalf("Register(spec) = %v", err)
	}
	pipe := &task.PipelineTask{APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "p", Name: "P", Subtype: "sequential", Enabled: true,
		Stages: []task.Stage{{Task: "s"}}}
	if err := env.reg.Register(pipe); err != nil {
		t.Fatal(err)
	}
	if err := env.engine.Register(pipe); err != nil {
		t.Fatalf("Register(pipeline) = %v", err)
	}
	// A bad pipeline (invalid subtype) is rejected.
	bad := &task.PipelineTask{APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "p2", Name: "P2", Subtype: "parallel",
		Stages: []task.Stage{{Task: "s"}}}
	if err := env.engine.Register(bad); err == nil {
		t.Fatal("expected subtype rejection")
	}
}
