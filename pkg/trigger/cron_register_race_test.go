package trigger

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/task"
)

// cronSpec builds a minimal cron-triggered task.Spec for the tests in this
// file, sharing raceSpec with webhookSpec (pkg/trigger/webhook_register_race_test.go).
func cronSpec(id, expr string, enabled bool) *task.Spec {
	return raceSpec(id, enabled, task.TriggerConfig{Cron: expr})
}

func cronJobsRow(t *testing.T, d db.DB, id string) (cronExpr string, nextRunAt int64, found bool) {
	t.Helper()
	_ = d.Query(context.Background(),
		`SELECT cron_expr, next_run_at FROM cron_jobs WHERE task_id=?`, []any{id},
		func(rows db.Scanner) error {
			if rows.Next() {
				found = true
				return rows.Scan(&cronExpr, &nextRunAt)
			}
			return nil
		},
	)
	return
}

// TestCronReRegister_NoOpKeepsSameCronEntry is the cron analogue of
// TestUnregisterTriggersKeeping_RetainsReclaimedPath: re-registering a task
// whose cron schedule is unchanged must not remove-and-re-add the
// robfig/cron entry (same EntryID survives) and must not rewrite the
// cron_jobs row (next_run_at survives byte-for-byte) — see issue #550.
func TestCronReRegister_NoOpKeepsSameCronEntry(t *testing.T) {
	eng, reg, d := raceEnv(t)
	spec := cronSpec("noop-cron", "* * * * *", true)
	if err := reg.Register(spec); err != nil {
		t.Fatalf("reg.Register: %v", err)
	}
	if err := eng.Register(spec); err != nil {
		t.Fatalf("eng.Register (first): %v", err)
	}

	eng.mu.Lock()
	firstArm, ok := eng.cronArmed[spec.ID]
	eng.mu.Unlock()
	if !ok {
		t.Fatal("cron entry not registered")
	}
	firstExpr, firstNextRunAt, found := cronJobsRow(t, d, spec.ID)
	if !found {
		t.Fatal("cron_jobs row not written after first Register")
	}

	// Re-register with the exact same spec (unchanged schedule) several times,
	// as the reconciler does on every unrelated content-hash change and as
	// Engine.Start does for every task at boot.
	for i := 0; i < 5; i++ {
		if err := eng.Register(spec); err != nil {
			t.Fatalf("eng.Register (re-register %d): %v", i, err)
		}
	}

	eng.mu.Lock()
	secondArm, ok := eng.cronArmed[spec.ID]
	eng.mu.Unlock()
	if !ok {
		t.Fatal("cron entry disappeared after no-op re-registration")
	}
	if secondArm.entry != firstArm.entry {
		t.Errorf("cron EntryID changed across no-op re-registrations: %v -> %v; "+
			"an unchanged schedule must not be torn down and re-added", firstArm.entry, secondArm.entry)
	}

	secondExpr, secondNextRunAt, found := cronJobsRow(t, d, spec.ID)
	if !found {
		t.Fatal("cron_jobs row disappeared after no-op re-registration")
	}
	if secondExpr != firstExpr {
		t.Errorf("cron_jobs.cron_expr changed on a no-op re-registration: %q -> %q", firstExpr, secondExpr)
	}
	if secondNextRunAt != firstNextRunAt {
		t.Errorf("cron_jobs.next_run_at was reset by a no-op re-registration: %d -> %d; "+
			"catchupMissedCronRuns relies on the prior session's persisted value surviving reloads",
			firstNextRunAt, secondNextRunAt)
	}
}

// TestCronReRegister_ScheduleChangeRearms verifies the asymmetric half of the
// fix: when the schedule string actually changes, the old cron.Cron entry
// really is torn down and a new one takes its place (EntryID changes), and
// the cron_jobs row is updated to the new expression.
func TestCronReRegister_ScheduleChangeRearms(t *testing.T) {
	eng, reg, d := raceEnv(t)
	spec := cronSpec("changing-cron", "* * * * *", true)
	if err := reg.Register(spec); err != nil {
		t.Fatalf("reg.Register: %v", err)
	}
	if err := eng.Register(spec); err != nil {
		t.Fatalf("eng.Register (first): %v", err)
	}

	eng.mu.Lock()
	firstArm := eng.cronArmed[spec.ID]
	eng.mu.Unlock()

	spec.Trigger.Cron = "*/5 * * * *"
	if err := reg.Register(spec); err != nil {
		t.Fatalf("reg.Register (updated): %v", err)
	}
	if err := eng.Register(spec); err != nil {
		t.Fatalf("eng.Register (updated): %v", err)
	}

	eng.mu.Lock()
	secondArm, ok := eng.cronArmed[spec.ID]
	eng.mu.Unlock()
	if !ok {
		t.Fatal("cron entry missing after schedule change")
	}
	if secondArm.entry == firstArm.entry {
		t.Error("EntryID did not change after the cron schedule changed — new schedule was never armed")
	}

	expr, _, found := cronJobsRow(t, d, spec.ID)
	if !found {
		t.Fatal("cron_jobs row missing after schedule change")
	}
	if expr != "*/5 * * * *" {
		t.Errorf("cron_jobs.cron_expr = %q; want the new schedule %q", expr, "*/5 * * * *")
	}
}

// TestCronReRegister_DisableTearsDownEntry verifies that disabling a
// previously-cron-scheduled task still removes its cron.Cron entry and
// cron_jobs row — the "removing/disabling still tears the entry down" half of
// the fix.
func TestCronReRegister_DisableTearsDownEntry(t *testing.T) {
	eng, reg, d := raceEnv(t)
	spec := cronSpec("toggle-cron", "* * * * *", true)
	if err := reg.Register(spec); err != nil {
		t.Fatalf("reg.Register: %v", err)
	}
	if err := eng.Register(spec); err != nil {
		t.Fatalf("eng.Register (enabled): %v", err)
	}

	spec.Enabled = false
	if err := reg.Register(spec); err != nil {
		t.Fatalf("reg.Register (disabled): %v", err)
	}
	if err := eng.Register(spec); err != nil {
		t.Fatalf("eng.Register (disabled): %v", err)
	}

	eng.mu.Lock()
	_, stillThere := eng.cronArmed[spec.ID]
	eng.mu.Unlock()
	if stillThere {
		t.Fatal("a disabled task kept its cron entry")
	}
	if _, _, found := cronJobsRow(t, d, spec.ID); found {
		t.Fatal("a disabled task kept its cron_jobs row")
	}
}

// TestCronNeverMissesTickDuringReRegister mirrors
// TestWebhookNeverFourOhFoursDuringReRegister (the merged #549 fix's race
// test) but for cron: it hammers Engine.Register for a task whose schedule
// never changes while the cron scheduler is live and actually ticking, and
// asserts at least one of the expected ticks actually fired.
//
// The schedule uses "@every 1s" — robfig/cron's ConstantDelaySchedule floors
// any @every delay to a minimum of one second (see constantdelay.go), so
// sub-second cron intervals aren't expressible through the real parser this
// engine uses. Each tick lands deterministically on a whole-second wall-clock
// boundary, so a several-second sleep reliably crosses multiple tick
// boundaries while the hammering goroutine runs concurrently.
//
// Before the #550 fix, every Register call — even one where the schedule
// string is byte-identical to what's already armed — unconditionally called
// e.cron.Remove(entryID) and then re-added a fresh entry via e.cron.AddFunc.
// A tight re-registration loop churns through that remove/re-add cycle far
// faster than the tick interval, so the entry attached to the cron scheduler
// is in a constant state of "just replaced" — a robfig/cron entry's next fire
// time is computed relative to the moment it was (re-)added, so replacing it
// before it ever gets a chance to fire pushes its next fire further and
// further into the future and ticks land in the remove/re-add gap and are
// silently dropped. The result is that almost no ticks fire while the
// hammering loop is running — reliably 0 in the pre-fix code. After the fix, a
// same-schedule re-registration leaves the armed entry completely untouched,
// so the scheduler ticks on schedule regardless of how often Register is
// called concurrently.
//
// The window is kept short (~2.2s, expecting ~2 ticks) and the assertion only
// requires >=1 fire — pkg/trigger is already close to the Makefile's 60s
// per-package test timeout (worse under -race), and 0-fires-pre-fix vs
// any-fires-post-fix is exactly as strong a signal as a larger margin: the
// pre-fix code doesn't fire "fewer" ticks, it starves the entry entirely.
func TestCronNeverMissesTickDuringReRegister(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping multi-second wall-clock cron test in -short mode")
	}
	eng, reg, _ := raceEnv(t)
	spec := cronSpec("hot-cron", "@every 1s", true)
	if err := reg.Register(spec); err != nil {
		t.Fatalf("reg.Register: %v", err)
	}
	if err := eng.Register(spec); err != nil {
		t.Fatalf("eng.Register: %v", err)
	}

	var fires atomic.Int64
	eng.AddRunFinishedHook(func(taskID, _, _, _ string, _ int64) {
		if taskID == spec.ID {
			fires.Add(1)
		}
	})

	eng.cron.Start()
	defer eng.cron.Stop()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				eng.Register(spec) //nolint:errcheck
			}
		}
	}()

	const testDuration = 2200 * time.Millisecond
	time.Sleep(testDuration)
	close(stop)
	wg.Wait()

	// Let any in-flight fires finalize before reading the counter.
	time.Sleep(100 * time.Millisecond)

	got := fires.Load()
	if got < 1 {
		t.Errorf("cron fired %d times in %s (expected at least 1 tick at a 1s interval) — "+
			"ticks are being dropped during concurrent re-registration", got, testDuration)
	}
}
