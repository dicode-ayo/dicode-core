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
