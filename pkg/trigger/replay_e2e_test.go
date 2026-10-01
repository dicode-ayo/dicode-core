package trigger

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
)

// TestReplay_FullPipeline exercises the complete v0.2.0 replay surface:
//  1. Persist input via the trigger engine (engine + deno runtime both wired).
//  2. Replay via registry.NewReplayer + trigger.NewReplayRunner(engine).
//  3. Assert the new run carries TriggerSource = "replay" and ParentRunID = original.
//  4. Assert the new run completes with StatusSuccess.
func TestReplay_FullPipeline(t *testing.T) {
	e := newTestEnv(t)

	runner := &fakeRunner{store: map[string]string{}}
	is := newFakeInputStore(runner, "fake-storage")
	// Wire into both the engine (persist-hook) and the deno runtime (IPC
	// server for delete_input / fetch). Both are required; wiring only the
	// engine leaves the IPC server without the store.
	e.engine.SetInputStore(is)
	e.denoRT.SetInputStore(is)

	spec := loadFixture(t, "replay-fullpipeline/echo-task", "")
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
	newRunID, err := replayer.Replay(context.Background(), originalRunID, "", "", "")
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

// A replayed run executes with the original fire-time params; a masked param
// falls back to its default and is never replaced by the redaction placeholder.
func TestReplay_RestoresFireParams(t *testing.T) {
	e := newTestEnv(t)

	runner := &fakeRunner{store: map[string]string{}}
	is := newFakeInputStore(runner, "fake-storage")
	e.engine.SetInputStore(is)
	e.denoRT.SetInputStore(is)

	spec := loadFixture(t, "replay-params/params-task", "")
	if err := e.reg.Register(spec); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	origID, err := e.engine.FireManual(ctx, "params-task", map[string]string{
		"greeting":  "hello",
		"api_token": "s3cret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if orig := waitForTerminal(t, e.engine, origID, 30*time.Second); orig.Status != registry.StatusSuccess {
		t.Fatalf("original status = %q", orig.Status)
	}

	replayer := registry.NewReplayer(e.reg, is, NewReplayRunner(e.engine))
	replayID, err := replayer.Replay(ctx, origID, "", "", "")
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	replayed := waitForTerminal(t, e.engine, replayID, 30*time.Second)
	if replayed.Status != registry.StatusSuccess {
		t.Fatalf("replay status = %q (%s), want success", replayed.Status, replayed.FailureReason)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(replayed.ReturnValue), &got); err != nil {
		t.Fatalf("return value %q: %v", replayed.ReturnValue, err)
	}
	if got["greeting"] != "hello" {
		t.Errorf("greeting = %q, want hello", got["greeting"])
	}
	if got["api_token"] != "default-token" {
		t.Errorf("api_token = %q, want default-token (masked param must not be restored)", got["api_token"])
	}

	// A replay of the replay carries the same params forward.
	again, err := replayer.Replay(ctx, replayID, "", "", "")
	if err != nil {
		t.Fatalf("Replay of replay: %v", err)
	}
	second := waitForTerminal(t, e.engine, again, 30*time.Second)
	if second.Status != registry.StatusSuccess {
		t.Fatalf("second replay status = %q (%s)", second.Status, second.FailureReason)
	}
	if !strings.Contains(second.ReturnValue, `"greeting":"hello"`) {
		t.Errorf("second replay return = %q, want greeting hello", second.ReturnValue)
	}
}
