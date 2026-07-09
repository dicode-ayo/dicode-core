package trigger

import (
	"context"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	denoruntime "github.com/dicode/dicode/pkg/runtime/deno"
	"github.com/dicode/dicode/pkg/task"
)

// TestDaemonSubprocessReapedOnTeardown guards #526. Registering a Daemon:true
// spec auto-starts a standalone daemon body; a pipeline whose terminal stage is
// that daemon spins up a second, stage-owned daemon run. The pipeline tests kill
// the stage run but never stop the auto-started standalone daemon, so before the
// fix its Deno subprocess was orphaned (reparented to init) when the test
// process exited. newTestEnv's teardown must now reap it.
//
// The test drives a real newTestEnv teardown in a subtest (the exact structure
// of TestPipelineDaemonTerminalStage) and then asserts no Deno subprocess
// survives. denoruntime.ActivePIDs counts subprocesses this test binary spawned
// (unaffected by unrelated Deno processes on the host) but is not scoped to a
// single engine, so the zero-survivor assertion is only valid while pkg/trigger
// tests run serially — a concurrent t.Parallel() test's subprocess would read
// as a false leak here.
func TestDaemonSubprocessReapedOnTeardown(t *testing.T) {
	if testing.Short() {
		t.Skip("requires Deno subprocess")
	}
	if n := len(denoruntime.ActivePIDs()); n != 0 {
		t.Skipf("%d Deno subprocess(es) already active from another test; cannot isolate", n)
	}

	t.Run("pipeline-with-daemon-terminal-stage", func(t *testing.T) {
		env := newTestEnv(t)
		dir := t.TempDir()

		stageA := writeTask(t, dir, "reap-a",
			`export default async function main() { return "a" }`,
			task.TriggerConfig{Manual: true})
		stageB := writeTask(t, dir, "reap-b",
			`export default async function main() { while (true) { await new Promise(r => setTimeout(r, 200)); } }`,
			task.TriggerConfig{Daemon: true, Restart: "never"})
		for _, s := range []*task.Spec{stageA, stageB} {
			if err := env.reg.Register(s); err != nil {
				t.Fatalf("reg.Register %s: %v", s.ID, err)
			}
			// Registering reap-b (Daemon:true) auto-starts a standalone daemon.
			if err := env.engine.Register(s); err != nil {
				t.Fatalf("eng.Register %s: %v", s.ID, err)
			}
		}

		pipe := &task.PipelineTask{
			APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
			ID: "reap-pipe", Name: "RP", Subtype: "sequential", Enabled: true,
			Trigger: task.PipelineTrigger{Manual: true},
			Stages:  []task.Stage{{Task: "reap-a"}, {Task: "reap-b"}},
		}
		if err := env.reg.Register(pipe); err != nil {
			t.Fatalf("reg.Register pipe: %v", err)
		}
		if err := env.engine.Register(pipe); err != nil {
			t.Fatalf("eng.Register pipe: %v", err)
		}

		parentRunID, err := env.engine.FireManual(context.Background(), "reap-pipe", nil)
		if err != nil {
			t.Fatalf("FireManual: %v", err)
		}

		daemonChild := findStageChild(t, env, parentRunID, "reap-b", registry.StatusRunning, 20*time.Second)
		// Standalone daemon + pipeline-stage daemon are both live now.
		if n := len(denoruntime.ActivePIDs()); n < 2 {
			t.Fatalf("expected >=2 live Deno subprocesses (standalone + stage), got %d", n)
		}
		// Kill only the pipeline stage run, exactly as the pipeline tests do. The
		// standalone daemon is left running — it is what leaked before the fix.
		if !env.engine.KillRun(daemonChild.ID) {
			t.Fatal("KillRun(daemonChild) returned false")
		}
		waitForTerminal(t, env.engine, parentRunID, 20*time.Second)
	})

	// The subtest's newTestEnv t.Cleanup runs on return and must have reaped the
	// orphaned standalone daemon subprocess.
	deadline := time.Now().Add(10 * time.Second)
	for len(denoruntime.ActivePIDs()) > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := len(denoruntime.ActivePIDs()); n != 0 {
		t.Fatalf("%d Deno subprocess(es) survived testEnv teardown; daemon runs not reaped", n)
	}
}
