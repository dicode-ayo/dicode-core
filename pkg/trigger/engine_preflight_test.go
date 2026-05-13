package trigger

// Tests for the cross-spec validation pass that the engine runs at
// registration time on tasks declaring `trigger.before`. Per-spec validation
// (see pkg/task.Spec.validate) can only enforce shape: it cannot check that
// the referenced task actually exists in the registry or that it is itself a
// one-shot task rather than another daemon. Both rules live here.

import (
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/task"
)

func TestRegister_BeforeUnknownTaskRejected(t *testing.T) {
	e := newTestEnv(t)
	daemon := &task.Spec{
		ID:      "d",
		Name:    "d",
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{Daemon: true, Before: []string{"missing"}},
		Enabled: true,
	}
	err := e.engine.Register(daemon)
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("expected error referencing unknown task, got %v", err)
	}
}

func TestRegister_BeforeDaemonRejected(t *testing.T) {
	e := newTestEnv(t)
	other := &task.Spec{
		ID:      "other-daemon",
		Name:    "other-daemon",
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{Daemon: true},
		Enabled: true,
	}
	if err := e.reg.Register(other); err != nil {
		t.Fatalf("seed reg.Register: %v", err)
	}
	target := &task.Spec{
		ID:      "d",
		Name:    "d",
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{Daemon: true, Before: []string{"other-daemon"}},
		Enabled: true,
	}
	err := e.engine.Register(target)
	if err == nil || !strings.Contains(err.Error(), "daemon") {
		t.Errorf("expected error rejecting daemon-as-before, got %v", err)
	}
}

// Cycle case: rejected structurally by the per-spec + cross-spec rules.
//
// trigger.before is only valid on daemon tasks (per-spec) AND it cannot
// reference a daemon (cross-spec). The only way a cycle could form is
// through a prereq task having its own trigger.before pointing back at the
// daemon — but only daemon tasks may have trigger.before. Therefore cycles
// are unreachable, and an explicit cycle-detection test is structurally
// impossible to write. The validator comment in validateBeforeRefs
// captures this reasoning so future readers don't add a redundant check.

// TestRegister_BeforeValid_NonDaemonTarget exercises the happy path: a
// daemon with a `before:` list referencing a one-shot task that exists in
// the registry registers without error.
func TestRegister_BeforeValid_NonDaemonTarget(t *testing.T) {
	e := newTestEnv(t)
	prereq := &task.Spec{
		ID:      "render",
		Name:    "render",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true},
		Enabled: true,
	}
	if err := e.reg.Register(prereq); err != nil {
		t.Fatalf("seed reg.Register: %v", err)
	}
	daemon := &task.Spec{
		ID:      "d",
		Name:    "d",
		Runtime: task.RuntimeDocker,
		Docker:  &task.DockerConfig{Image: "alpine"},
		Trigger: task.TriggerConfig{Daemon: true, Before: []string{"render"}},
		Enabled: true,
	}
	if err := e.engine.Register(daemon); err != nil {
		t.Errorf("unexpected error on valid before-list: %v", err)
	}
}
