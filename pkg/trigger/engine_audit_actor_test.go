package trigger

import (
	"context"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/audit"
	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// newAuditActorEnv builds an engine with the audit store wired (SetDB) but no
// executor — these tests only assert on the run_triggered event emitted by
// startRun, which fires before dispatch, so the run itself failing with
// "no executor" is irrelevant.
func newAuditActorEnv(t *testing.T) (*Engine, *registry.Registry) {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)
	eng := New(reg, nil, zap.NewNop())
	eng.SetDB(d)
	return eng, reg
}

// fireAndWait fires the manual run and blocks until it reaches a terminal
// state so the background goroutine cannot race the test's DB teardown.
func fireAndWait(t *testing.T, eng *Engine, taskID, actor string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var runID string
	var err error
	if actor != "" {
		runID, err = eng.FireManualWithActor(ctx, taskID, nil, actor)
	} else {
		runID, err = eng.FireManual(ctx, taskID, nil)
	}
	if err != nil {
		t.Fatalf("fire %s: %v", taskID, err)
	}
	if _, err := eng.WaitRun(ctx, runID); err != nil {
		t.Fatalf("WaitRun %s: %v", runID, err)
	}
	return runID
}

// TestFireManualWithActor_RecordsAuditActor verifies the run_triggered audit
// event (#45) carries the operator principal: actor_id is the TriggerActor
// for actor-carrying manual fires, and stays empty for plain FireManual
// (where there is no parent run either).
func TestFireManualWithActor_RecordsAuditActor(t *testing.T) {
	eng, reg := newAuditActorEnv(t)
	spec := &task.Spec{
		ID:      "actor-task",
		Name:    "actor-task",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true},
		Timeout: 5 * time.Second,
		Enabled: true,
	}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("register: %v", err)
	}

	withActor := fireAndWait(t, eng, "actor-task", "203.0.113.7")
	plain := fireAndWait(t, eng, "actor-task", "")

	events, err := eng.audit.Query(context.Background(), audit.Filter{EventType: audit.EventRunTriggered})
	if err != nil {
		t.Fatalf("audit query: %v", err)
	}
	byRun := map[string]audit.Event{}
	for _, ev := range events {
		byRun[ev.RunID] = ev
	}

	ev, ok := byRun[withActor]
	if !ok {
		t.Fatalf("no run_triggered event for actor run %s", withActor)
	}
	if ev.ActorKind != string(registry.TriggerManual) {
		t.Errorf("actor run actor_kind = %q, want %q", ev.ActorKind, registry.TriggerManual)
	}
	if ev.ActorID != "203.0.113.7" {
		t.Errorf("actor run actor_id = %q, want %q", ev.ActorID, "203.0.113.7")
	}

	ev, ok = byRun[plain]
	if !ok {
		t.Fatalf("no run_triggered event for plain run %s", plain)
	}
	if ev.ActorID != "" {
		t.Errorf("plain FireManual actor_id = %q, want empty", ev.ActorID)
	}
}
