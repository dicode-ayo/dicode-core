package trigger

import (
	"context"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// notifyLikeSpec mirrors buildin/notify: two required params with no default,
// plus an optional one.
func notifyLikeSpec(id string) *task.Spec {
	return &task.Spec{
		ID:      id,
		Name:    id,
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true},
		Timeout: 10 * time.Second,
		Enabled: true,
		Params: task.Params{
			{Name: "title", Type: "string", Required: true},
			{Name: "body", Type: "string", Required: true},
			{Name: "priority", Type: "string", Default: "default"},
		},
	}
}

// TestFire_MissingRequiredParams_FailsBeforeDispatch pins #800: `required: true`
// is enforced on the fire path, not only by `dicode task test`. A run missing a
// required param never reaches the executor and settles as a failed run whose
// fail_reason names the fields.
func TestFire_MissingRequiredParams_FailsBeforeDispatch(t *testing.T) {
	tests := []struct {
		name       string
		params     map[string]string
		wantReason string
	}{
		{"none supplied", nil, "params_invalid: title, body are required"},
		{"one supplied", map[string]string{"title": "Approval pending"}, "params_invalid: body is required"},
		{"empty value", map[string]string{"title": "Approval pending", "body": ""}, "params_invalid: body is required"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
			if err != nil {
				t.Fatalf("open db: %v", err)
			}
			t.Cleanup(func() { d.Close() })
			reg := registry.New(d)

			exec := &chainTargetCountingExecutor{}
			eng := New(reg, exec, zap.NewNop())

			spec := notifyLikeSpec("notify")
			if err := reg.Register(spec); err != nil {
				t.Fatalf("register: %v", err)
			}

			runID, err := eng.FireManual(context.Background(), "notify", tc.params)
			if err != nil {
				t.Fatalf("FireManual: %v", err)
			}
			if _, err := eng.WaitRun(context.Background(), runID); err != nil {
				t.Fatalf("WaitRun: %v", err)
			}

			run, err := reg.GetRun(context.Background(), runID)
			if err != nil {
				t.Fatalf("GetRun: %v", err)
			}
			if run.Status != registry.StatusFailure {
				t.Errorf("status = %q, want %q", run.Status, registry.StatusFailure)
			}
			if run.FailureReason != tc.wantReason {
				t.Errorf("fail_reason = %q, want %q", run.FailureReason, tc.wantReason)
			}
			if n := exec.count("notify"); n != 0 {
				t.Errorf("executor dispatched %d time(s); the run must fail before dispatch", n)
			}
		})
	}
}

// TestFire_SatisfiedRequiredParams_Dispatches is the negative half: a declared
// default and a fire-time override each satisfy `required`, and an undeclared
// key rides through rather than being rejected the way ValidateParams' closed
// schema would reject it.
func TestFire_SatisfiedRequiredParams_Dispatches(t *testing.T) {
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)

	exec := &chainTargetCountingExecutor{}
	eng := New(reg, exec, zap.NewNop())

	spec := notifyLikeSpec("notify")
	spec.Params[1].Default = "Body from the spec"
	if err := reg.Register(spec); err != nil {
		t.Fatalf("register: %v", err)
	}

	runID, err := eng.FireManual(context.Background(), "notify", map[string]string{
		"title": "Approval pending",
		"event": "approval_pending",
	})
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	if _, err := eng.WaitRun(context.Background(), runID); err != nil {
		t.Fatalf("WaitRun: %v", err)
	}

	run, err := reg.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != registry.StatusSuccess {
		t.Errorf("status = %q (fail_reason %q), want %q", run.Status, run.FailureReason, registry.StatusSuccess)
	}
	if n := exec.count("notify"); n != 1 {
		t.Errorf("executor dispatched %d time(s), want 1", n)
	}
}
