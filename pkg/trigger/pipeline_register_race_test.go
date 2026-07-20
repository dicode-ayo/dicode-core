package trigger

import (
	"testing"

	"github.com/dicode/dicode/pkg/task"
)

// pipelineRaceStage registers a trivial manual-triggered kind: Task stage
// (docker runtime + raceEnv's immediateExec, so no real subprocess is ever
// launched) that the pipelines in this file reference.
func pipelineRaceStage(t *testing.T, eng *Engine, reg registrar, id string) {
	t.Helper()
	stage := raceSpec(id, true, task.TriggerConfig{Manual: true})
	if err := reg.Register(stage); err != nil {
		t.Fatalf("reg.Register(stage %q): %v", id, err)
	}
	if err := eng.Register(stage); err != nil {
		t.Fatalf("eng.Register(stage %q): %v", id, err)
	}
}

// registrar is the subset of *registry.Registry used by pipelineRaceStage —
// declared narrowly so this file doesn't need to import pkg/registry just for
// a type name.
type registrar interface {
	Register(k task.Kinded) error
}

// cronPipeline builds a minimal cron-triggered PipelineTask referencing a
// single stage, for the tests in this file.
func cronPipeline(id, expr string, enabled bool, stage string) *task.PipelineTask {
	return &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: id, Name: id, Subtype: "sequential", Enabled: enabled,
		Trigger: task.PipelineTrigger{Cron: expr},
		Stages:  []task.Stage{{Task: stage}},
	}
}

// TestPipelineCronReRegister_NoOpKeepsSameCronEntry is the PipelineTask
// analogue of TestCronReRegister_NoOpKeepsSameCronEntry (see
// cron_register_race_test.go): re-registering a pipeline whose cron schedule
// is unchanged must not remove-and-re-add the robfig/cron entry.
//
// This is the regression test for review finding 1: registerPipeline used to
// route straight to registerPipelineCron without ever calling
// unregisterTriggersKeeping, so this test fails without the fix (a NEW
// cron.EntryID is added on every re-registration, growing eng.cron.Entries()
// without bound) and passes with it.
func TestPipelineCronReRegister_NoOpKeepsSameCronEntry(t *testing.T) {
	eng, reg, _ := raceEnv(t)
	pipelineRaceStage(t, eng, reg, "s")

	pipe := cronPipeline("noop-pipe", "0 0 * * *", true, "s")
	if err := reg.Register(pipe); err != nil {
		t.Fatalf("reg.Register(pipe): %v", err)
	}
	if err := eng.Register(pipe); err != nil {
		t.Fatalf("eng.Register(pipe) (first): %v", err)
	}

	eng.mu.Lock()
	firstArm, ok := eng.cronArmed[pipe.ID]
	eng.mu.Unlock()
	if !ok {
		t.Fatal("cron entry not registered for cron-triggered pipeline")
	}

	for i := 0; i < 5; i++ {
		if err := eng.Register(pipe); err != nil {
			t.Fatalf("eng.Register(pipe) (re-register %d): %v", i, err)
		}
	}

	eng.mu.Lock()
	secondArm, ok := eng.cronArmed[pipe.ID]
	nEntries := len(eng.cron.Entries())
	eng.mu.Unlock()
	if !ok {
		t.Fatal("cron entry disappeared after no-op pipeline re-registration")
	}
	if secondArm.entry != firstArm.entry {
		t.Errorf("cron EntryID changed across no-op pipeline re-registrations: %v -> %v; "+
			"an unchanged schedule must not be torn down and re-added", firstArm.entry, secondArm.entry)
	}
	if nEntries != 1 {
		t.Errorf("eng.cron.Entries() has %d entries after no-op re-registration; want 1 — "+
			"a no-op reload must never add a duplicate cron.Cron entry", nEntries)
	}
}

// TestPipelineCronReRegister_ScheduleChangeRearms is the PipelineTask
// analogue of TestCronReRegister_ScheduleChangeRearms: when a pipeline's cron
// schedule genuinely changes, the OLD cron.Cron entry must be torn down (not
// merely superseded/orphaned) and a new one takes its place.
//
// This is the concrete regression described in review finding 1: before the
// fix, registerPipeline called registerPipelineCron unconditionally without
// ever calling unregisterTriggersKeeping, so scheduleCron's cron.AddFunc for
// the new schedule left the old cron.EntryID permanently attached to the
// scheduler — both entries would fire forever. The len(eng.cron.Entries())
// assertion is the one that catches that leak; asserting only "EntryID
// changed" would pass even with the leak.
func TestPipelineCronReRegister_ScheduleChangeRearms(t *testing.T) {
	eng, reg, _ := raceEnv(t)
	pipelineRaceStage(t, eng, reg, "s")

	pipe := cronPipeline("changing-pipe", "0 0 * * *", true, "s")
	if err := reg.Register(pipe); err != nil {
		t.Fatalf("reg.Register(pipe): %v", err)
	}
	if err := eng.Register(pipe); err != nil {
		t.Fatalf("eng.Register(pipe) (first): %v", err)
	}

	eng.mu.Lock()
	firstArm := eng.cronArmed[pipe.ID]
	eng.mu.Unlock()

	pipe.Trigger.Cron = "0 1 * * *"
	if err := reg.Register(pipe); err != nil {
		t.Fatalf("reg.Register(pipe) (updated): %v", err)
	}
	if err := eng.Register(pipe); err != nil {
		t.Fatalf("eng.Register(pipe) (updated): %v", err)
	}

	eng.mu.Lock()
	secondArm, ok := eng.cronArmed[pipe.ID]
	nEntries := len(eng.cron.Entries())
	eng.mu.Unlock()
	if !ok {
		t.Fatal("cron entry missing after pipeline schedule change")
	}
	if secondArm.entry == firstArm.entry {
		t.Error("EntryID did not change after the pipeline's cron schedule changed — new schedule was never armed")
	}
	if nEntries != 1 {
		t.Errorf("eng.cron.Entries() has %d entries after a genuine schedule change; want exactly 1 — "+
			"the old cron.EntryID was orphaned instead of being removed (issue #550, pipeline case)", nEntries)
	}
}

// TestPipelineCronReRegister_DisableTearsDownEntry verifies that disabling a
// previously-cron-scheduled pipeline removes its cron.Cron entry — the
// pipeline analogue of TestCronReRegister_DisableTearsDownEntry.
func TestPipelineCronReRegister_DisableTearsDownEntry(t *testing.T) {
	eng, reg, _ := raceEnv(t)
	pipelineRaceStage(t, eng, reg, "s")

	pipe := cronPipeline("toggle-pipe", "0 0 * * *", true, "s")
	if err := reg.Register(pipe); err != nil {
		t.Fatalf("reg.Register(pipe): %v", err)
	}
	if err := eng.Register(pipe); err != nil {
		t.Fatalf("eng.Register(pipe) (enabled): %v", err)
	}

	pipe.Enabled = false
	if err := reg.Register(pipe); err != nil {
		t.Fatalf("reg.Register(pipe) (disabled): %v", err)
	}
	if err := eng.Register(pipe); err != nil {
		t.Fatalf("eng.Register(pipe) (disabled): %v", err)
	}

	eng.mu.Lock()
	_, stillThere := eng.cronArmed[pipe.ID]
	nEntries := len(eng.cron.Entries())
	eng.mu.Unlock()
	if stillThere {
		t.Fatal("a disabled pipeline kept its cron entry")
	}
	if nEntries != 0 {
		t.Errorf("eng.cron.Entries() has %d entries after disabling the only cron pipeline; want 0", nEntries)
	}
}

// TestPipelineWebhookReRegister_PathChangeReleasesOldPath is the PipelineTask
// analogue of the webhook half of review finding 1: registerPipeline used to
// call registerWebhookPath unconditionally, and registerWebhookPath only ever
// adds/overwrites e.webhooks[path] — never removing a previously-claimed path
// for the same ID. Editing a pipeline's trigger.webhook must release the old
// path so it doesn't stay permanently routed to the pipeline.
func TestPipelineWebhookReRegister_PathChangeReleasesOldPath(t *testing.T) {
	eng, reg, _ := raceEnv(t)
	pipelineRaceStage(t, eng, reg, "s")

	pipe := &task.PipelineTask{
		APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "wp", Name: "WP", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Webhook: "/hooks/old"},
		Stages:  []task.Stage{{Task: "s"}},
	}
	if err := reg.Register(pipe); err != nil {
		t.Fatalf("reg.Register(pipe): %v", err)
	}
	if err := eng.Register(pipe); err != nil {
		t.Fatalf("eng.Register(pipe) (first): %v", err)
	}

	eng.mu.Lock()
	got := eng.webhooks["/hooks/old"]
	eng.mu.Unlock()
	if got != "wp" {
		t.Fatalf("webhook path not claimed by pipeline before the change: got %q, want wp", got)
	}

	pipe.Trigger.Webhook = "/hooks/new"
	if err := reg.Register(pipe); err != nil {
		t.Fatalf("reg.Register(pipe) (updated): %v", err)
	}
	if err := eng.Register(pipe); err != nil {
		t.Fatalf("eng.Register(pipe) (updated): %v", err)
	}

	eng.mu.Lock()
	_, oldStillThere := eng.webhooks["/hooks/old"]
	newOwner := eng.webhooks["/hooks/new"]
	eng.mu.Unlock()
	if oldStillThere {
		t.Error("webhooks[/hooks/old] survived a pipeline webhook path change — old path leaked (issue #550, pipeline case)")
	}
	if newOwner != "wp" {
		t.Errorf("webhooks[/hooks/new] = %q; want wp", newOwner)
	}
}
