// pkg/trigger/engine_guardrails_test.go
package trigger

import (
	"testing"
	"time"
)

func TestChainGuards_CooldownActive(t *testing.T) {
	g := newChainGuards()
	taskID := "process-payment"

	// First fire: cooldown not active.
	if g.cooldownActive(taskID, 10*time.Minute, time.Now()) {
		t.Error("cooldown should not be active before any fire")
	}
	g.recordChainFire(taskID, time.Now())

	// Same instant: active.
	if !g.cooldownActive(taskID, 10*time.Minute, time.Now()) {
		t.Error("cooldown must be active immediately after fire")
	}

	// 11 minutes later: lapsed.
	later := time.Now().Add(11 * time.Minute)
	if g.cooldownActive(taskID, 10*time.Minute, later) {
		t.Error("cooldown should expire after window")
	}

	// Different task: independent.
	if g.cooldownActive("other-task", 10*time.Minute, time.Now()) {
		t.Error("cooldown is per-task; other-task should be free")
	}
}

func TestChainGuards_ConcurrencyPerTask(t *testing.T) {
	g := newChainGuards()
	failingTask := "process-payment"

	// Cap=1: first acquire succeeds, second is denied.
	if !g.acquireSlot(failingTask, 1, 3) {
		t.Error("first acquire should succeed")
	}
	if g.acquireSlot(failingTask, 1, 3) {
		t.Error("second acquire should be denied at per-task cap=1")
	}

	// Release one, acquire succeeds again.
	g.releaseSlot(failingTask)
	if !g.acquireSlot(failingTask, 1, 3) {
		t.Error("post-release acquire should succeed")
	}
}

func TestChainGuards_ConcurrencyGlobal(t *testing.T) {
	g := newChainGuards()

	// Per-task cap=10 (effectively no per-task limit). Global cap=2.
	if !g.acquireSlot("a", 10, 2) {
		t.Fatal("a-1 acquire should succeed")
	}
	if !g.acquireSlot("b", 10, 2) {
		t.Fatal("b-1 acquire should succeed")
	}
	if g.acquireSlot("c", 10, 2) {
		t.Error("c-1 should be denied at global=2 (a+b in flight)")
	}
	g.releaseSlot("a")
	if !g.acquireSlot("c", 10, 2) {
		t.Error("c-1 should succeed after a releases")
	}
}

func TestChainGuards_StormTrip(t *testing.T) {
	g := newChainGuards()
	scope := "user-tasks"
	rate := 3
	window := 1 * time.Minute
	suppress := 30 * time.Minute

	now := time.Now()
	for i := 0; i < 3; i++ {
		if g.stormSuppressed(scope, suppress, now) {
			t.Fatalf("breaker tripped early at i=%d", i)
		}
		g.observeChainFire(scope, rate, window, suppress, now)
	}

	// 4th observation in the window trips the breaker.
	g.observeChainFire(scope, rate, window, suppress, now)
	if !g.stormSuppressed(scope, suppress, now) {
		t.Error("breaker should be tripped after 4 fires in window")
	}

	// 31 minutes later: untripped.
	later := now.Add(31 * time.Minute)
	if g.stormSuppressed(scope, suppress, later) {
		t.Error("breaker should clear after suppress duration")
	}

	// Different scope: independent.
	if g.stormSuppressed("other-source", suppress, now) {
		t.Error("storm scope should isolate sources")
	}
}
