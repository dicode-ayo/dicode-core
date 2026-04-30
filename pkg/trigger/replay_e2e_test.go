package trigger

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
)

// TestReplay_FullPipeline exercises the complete v0.2.0 replay surface:
//  1. Persist input via the trigger engine (engine + deno runtime both wired).
//  2. Replay via registry.NewReplayer + trigger.NewReplayRunner(engine).
//  3. Assert the new run carries TriggerSource = "replay" and ParentRunID = original.
//  4. Assert the new run completes with StatusSuccess.
func TestReplay_FullPipeline(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)

	runner := &fakeRunner{store: map[string]string{}}
	is := newFakeInputStore(runner, "fake-storage")
	// Wire into both the engine (persist-hook) and the deno runtime (IPC
	// server for delete_input / fetch). Both are required; wiring only the
	// engine leaves the IPC server without the store.
	e.engine.SetInputStore(is)
	e.denoRT.SetInputStore(is)

	spec := writeTask(t, dir, "echo-task",
		`export default async function main(opts) {
			return opts && opts.input ? opts.input : "no-input"
		}`,
		task.TriggerConfig{Manual: true})
	if err := e.reg.Register(spec); err != nil {
		t.Fatal(err)
	}

	// Fire the original run with params; verify input is persisted.
	originalRunID, err := e.engine.FireManual(context.Background(), "echo-task", map[string]string{"key1": "value1"})
	if err != nil {
		t.Fatal(err)
	}
	waitForTerminal(t, e.engine, originalRunID, 30*time.Second)

	got, err := e.reg.GetRun(context.Background(), originalRunID)
	if err != nil {
		t.Fatal(err)
	}
	if got.InputStorageKey == "" {
		t.Fatal("input not persisted on original run")
	}

	// Replay via the Replayer + adapter.
	replayer := registry.NewReplayer(e.reg, is, NewReplayRunner(e.engine))
	newRunID, err := replayer.Replay(context.Background(), originalRunID, "")
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}

	replayed := waitForTerminal(t, e.engine, newRunID, 30*time.Second)
	if replayed.TriggerSource != "replay" {
		t.Errorf("TriggerSource = %q, want replay", replayed.TriggerSource)
	}
	if replayed.ParentRunID != originalRunID {
		t.Errorf("ParentRunID = %q, want %q", replayed.ParentRunID, originalRunID)
	}
	if replayed.Status != registry.StatusSuccess {
		t.Errorf("replay run status = %q, want success", replayed.Status)
	}

	// Sanity: if the runtime surfaces a ReturnValue, it should be valid JSON.
	if rv := replayed.ReturnValue; rv != "" {
		var parsed any
		if err := json.Unmarshal([]byte(rv), &parsed); err != nil {
			t.Logf("ReturnValue not JSON (non-fatal): %s", rv)
		}
	}
}
