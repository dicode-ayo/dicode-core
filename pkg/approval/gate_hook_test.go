package approval

import (
	"sync"
	"testing"

	"github.com/dicode/dicode/pkg/task"
)

// hookCall records one pending-hook invocation.
type hookCall struct {
	id   string
	hash string
}

func TestPendingHookFiresOnNewHold(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	g.SetHashFunc(func(task.Kinded) (string, error) { return "h1", nil })
	var mu sync.Mutex
	var calls []hookCall
	g.SetPendingHook(func(k task.Kinded, hash string) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, hookCall{k.TaskID(), hash})
	})

	spec := &task.Spec{ID: "repo/deploy"}
	if armed, _ := g.Admit(spec); armed {
		t.Fatal("untrusted task must be pending")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || calls[0] != (hookCall{"repo/deploy", "h1"}) {
		t.Fatalf("hook calls = %+v, want one {repo/deploy h1}", calls)
	}
}

func TestPendingHookSkipsUnchangedReAdmit(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	g.SetHashFunc(func(task.Kinded) (string, error) { return "h1", nil })
	var mu sync.Mutex
	var n int
	g.SetPendingHook(func(task.Kinded, string) { mu.Lock(); n++; mu.Unlock() })

	spec := &task.Spec{ID: "repo/deploy"}
	g.Admit(spec) // new hold → fires
	g.Admit(spec) // unchanged re-admit (reconcile poll) → must NOT re-fire
	g.Admit(spec)
	mu.Lock()
	defer mu.Unlock()
	if n != 1 {
		t.Fatalf("hook fired %d times, want 1 (only on transition)", n)
	}
}

func TestPendingHookFiresOnHashChange(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	hash := "h1"
	g.SetHashFunc(func(task.Kinded) (string, error) { return hash, nil })
	var mu sync.Mutex
	var calls []hookCall
	g.SetPendingHook(func(k task.Kinded, h string) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, hookCall{k.TaskID(), h})
	})

	spec := &task.Spec{ID: "repo/deploy"}
	g.Admit(spec) // h1 hold
	hash = "h2"   // content changed while still pending
	g.Admit(spec) // h2 → must re-fire
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || calls[1].hash != "h2" {
		t.Fatalf("hook calls = %+v, want a second fire at h2", calls)
	}
}

func TestPendingHookNotFiredForTrustedTask(t *testing.T) {
	policy := enabledPolicy()
	policy.TrustedTasks["repo/deploy"] = true
	g, _, _ := newTestGate(t, policy)
	g.SetHashFunc(func(task.Kinded) (string, error) { return "h1", nil })
	var fired bool
	g.SetPendingHook(func(task.Kinded, string) { fired = true })

	if armed, _ := g.Admit(&task.Spec{ID: "repo/deploy"}); !armed {
		t.Fatal("trusted task must arm")
	}
	if fired {
		t.Fatal("hook must not fire for an auto-approved task")
	}
}

// TestPendingHookFiresOnEnabledTransition is a regression for #822: a task
// that first goes pending while disabled (task.yaml or a taskset override)
// fires the hook once, but if the operator later flips only the enabled
// override — without touching the task's content — the hash never changes
// (enabled is deliberately excluded from ContentHash; see
// resolvedSecurityFields). Before the fix, Admit's changed condition only
// looked at the hash, so this re-admit never re-fired the hook and
// notify_task stayed silent forever for a task that now genuinely blocks
// real triggers. The hook must fire again here, observably at enabled=true.
func TestPendingHookFiresOnEnabledTransition(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	g.SetHashFunc(func(task.Kinded) (string, error) { return "h1", nil })
	var mu sync.Mutex
	var calls []struct {
		hash    string
		enabled bool
	}
	g.SetPendingHook(func(k task.Kinded, hash string) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, struct {
			hash    string
			enabled bool
		}{hash, k.IsEnabled()})
	})

	spec := &task.Spec{ID: "repo/deploy", Enabled: false}
	if armed, _ := g.Admit(spec); armed {
		t.Fatal("disabled task must still be held pending")
	}

	// Operator flips the taskset override to enabled: true. Content — and
	// therefore hash — is unchanged.
	spec.Enabled = true
	if armed, _ := g.Admit(spec); armed {
		t.Fatal("expected still pending")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("hook fired %d times, want 2 (once on the initial hold, once on the enabled transition); calls=%+v", len(calls), calls)
	}
	if calls[0].enabled {
		t.Fatalf("first call enabled = true, want false (initial hold while disabled)")
	}
	if !calls[1].enabled {
		t.Fatalf("second call enabled = false, want true (re-fired on enabled transition)")
	}
	if calls[1].hash != "h1" {
		t.Fatalf("second call hash = %q, want h1 (hash unchanged across the transition)", calls[1].hash)
	}
}

func TestPendingHookNilSafe(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	g.SetHashFunc(func(task.Kinded) (string, error) { return "h1", nil })
	// No hook installed; Admit must not panic on the pending path.
	if armed, _ := g.Admit(&task.Spec{ID: "repo/deploy"}); armed {
		t.Fatal("untrusted task must be pending")
	}
}
