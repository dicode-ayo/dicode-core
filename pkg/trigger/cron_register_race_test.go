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

// TestScheduleCron_StaleTickCallbackDoesNotClobberNewSchedule exercises the
// UPDATE cron_jobs ... WHERE task_id=? AND cron_expr=? guard directly at the
// SQL level (not via real goroutine timing, which would be flaky): robfig/cron
// runs each tick's Job.Run() in its own goroutine, and e.cron.Remove doesn't
// cancel one already in flight, so a tick dispatched under an OLD schedule can
// still be persisting its next_run_at after a genuine schedule change has
// already moved the row to a NEW cron_expr. Without constraining the UPDATE by
// cron_expr, that stale write would silently overwrite the new schedule's
// next_run_at with a value computed from the expression the old closure
// captured. This directly simulates that interleaving: seed the row as if the
// new schedule already registered, then run the exact stale-closure UPDATE for
// the old schedule and assert it is a no-op.
func TestScheduleCron_StaleTickCallbackDoesNotClobberNewSchedule(t *testing.T) {
	_, _, d := raceEnv(t)
	ctx := context.Background()
	const id = "reschedule-me"

	// The new schedule ("*/5 * * * *") has already been armed and its row
	// written by a fresh registerCron/scheduleCron call.
	if err := d.Exec(ctx,
		`INSERT INTO cron_jobs(task_id,cron_expr,next_run_at) VALUES(?,?,?)`,
		id, "*/5 * * * *", int64(2000),
	); err != nil {
		t.Fatalf("seed cron_jobs row: %v", err)
	}

	// A tick dispatched under the OLD schedule ("* * * * *") before the
	// reschedule finishes running now and reaches the same UPDATE
	// scheduleCron's cron.AddFunc closure performs, still holding the old
	// cronExpr it was dispatched with.
	if err := d.Exec(ctx,
		`UPDATE cron_jobs SET last_run_at=?, next_run_at=? WHERE task_id=? AND cron_expr=?`,
		int64(9999), int64(9999), id, "* * * * *",
	); err != nil {
		t.Fatalf("stale UPDATE: %v", err)
	}

	cronExpr, nextRunAt, found := cronJobsRow(t, d, id)
	if !found {
		t.Fatal("cron_jobs row disappeared")
	}
	if cronExpr != "*/5 * * * *" || nextRunAt != 2000 {
		t.Errorf("stale tick callback clobbered the row: cron_expr=%q next_run_at=%d, want cron_expr=%q next_run_at=2000 (unchanged)",
			cronExpr, nextRunAt, "*/5 * * * *")
	}

	// A tick dispatched under the CURRENT schedule must still persist normally.
	if err := d.Exec(ctx,
		`UPDATE cron_jobs SET last_run_at=?, next_run_at=? WHERE task_id=? AND cron_expr=?`,
		int64(3000), int64(3300), id, "*/5 * * * *",
	); err != nil {
		t.Fatalf("current-schedule UPDATE: %v", err)
	}
	cronExpr, nextRunAt, found = cronJobsRow(t, d, id)
	if !found {
		t.Fatal("cron_jobs row disappeared")
	}
	if cronExpr != "*/5 * * * *" || nextRunAt != 3300 {
		t.Errorf("current-schedule tick callback failed to persist: cron_expr=%q next_run_at=%d, want cron_expr=%q next_run_at=3300",
			cronExpr, nextRunAt, "*/5 * * * *")
	}
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
// asserts that every tick the scheduler dispatched actually reached the task.
//
// The schedule uses "@every 1s" — robfig/cron's ConstantDelaySchedule floors
// any @every delay to a minimum of one second (see constantdelay.go), so
// sub-second cron intervals aren't expressible through the real parser this
// engine uses. Each tick lands deterministically on a whole-second wall-clock
// boundary, so a ~2s sleep reliably crosses two or three tick boundaries while
// the hammering goroutine runs concurrently.
//
// Before the #550 fix, every Register call — even one where the schedule
// string is byte-identical to what's already armed — unconditionally called
// e.cron.Remove(entryID) and then re-added a fresh entry via e.cron.AddFunc.
// A tight re-registration loop churns through that remove/re-add cycle far
// faster than the tick interval, so at any given instant the task's entry is
// either absent from the scheduler or freshly re-added with its next fire
// pushed to the *following* boundary; a tick whose boundary lands in that gap
// is silently dropped. Empirically the pre-fix engine loses most ticks (0 or 1
// fires out of 2–3 boundaries per run) but not reliably all of them, so an
// absolute "at least N fires" floor is either flaky (N too high) or blind to
// the regression (N too low). After the fix, a same-schedule re-registration
// leaves the armed entry completely untouched, so the scheduler ticks on
// schedule regardless of how often Register is called concurrently.
//
// The oracle is therefore a control entry: a second "@every 1s" func armed
// directly on the same cron.Cron that is never re-registered. Both entries
// share one scheduler loop and one timer, so at every boundary they are due
// together and are dispatched in the same run-loop iteration. With the fix the
// hot task's entry is never removed, so its fire count keeps pace with the
// control's exactly; without the fix any tick lost in a remove/re-add gap
// shows up as hot < control. Comparing against the control instead of the
// wall clock also keeps the window short (~2.2s) without a timing margin —
// pkg/trigger is already close to the Makefile's 60s per-package test timeout,
// worse under -race.
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

	// Control entry: same schedule, same scheduler, never touched by Register.
	// Its count is the number of boundaries the scheduler actually dispatched.
	var controlTicks atomic.Int64
	if _, err := eng.cron.AddFunc("@every 1s", func() { controlTicks.Add(1) }); err != nil {
		t.Fatalf("arm control cron entry: %v", err)
	}

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

	// The control increments synchronously at dispatch; the hot task's fire is
	// counted only once its (immediate) run finishes. Let a tick dispatched
	// right at the end of the window finalize before comparing the two.
	time.Sleep(250 * time.Millisecond)

	got, want := fires.Load(), controlTicks.Load()
	if want < 1 {
		t.Fatalf("control cron entry never ticked in %s — scheduler did not run, test window too short", testDuration)
	}
	if got < want {
		t.Errorf("cron fired %d times in %s but the scheduler dispatched %d ticks — "+
			"ticks are being dropped during concurrent re-registration", got, testDuration, want)
	}
}
