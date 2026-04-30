// pkg/trigger/auto_fix_e2e_test.go
package trigger

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
)

// TestAutoFix_E2E_HappyPath wires the engine, registry, input store, and chain
// dispatcher through a controlled-failure path:
//  1. A webhook task fails (controlled).
//  2. on_failure_chain fires buildin/auto-fix.
//  3. The chain dispatcher fires the stub auto-fix task.
//  4. Assert: the original run has StatusFailure, auto-fix runs and succeeds,
//     and its return value contains "pr_url".
//
// The full agent loop (real AI, Deno, LLM) is exercised by manual smoke testing.
// This test stubs out buildin/auto-fix to a trivially-passing task so no
// external dependencies are required.
//
// Gated on DICODE_E2E=1 so default unit-test runs are unaffected.
func TestAutoFix_E2E_HappyPath(t *testing.T) {
	if os.Getenv("DICODE_E2E") == "" {
		t.Skip("DICODE_E2E not set — skipping integration test")
	}
	dir := t.TempDir()
	e := newTestEnv(t)

	// Stub gh on PATH so any shell invocation of `gh` returns a fake PR URL.
	stubDir := filepath.Join(dir, "stub-bin")
	if err := os.MkdirAll(stubDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ghStub := filepath.Join(stubDir, "gh")
	if err := os.WriteFile(ghStub,
		[]byte("#!/bin/sh\necho https://github.com/example/repo/pull/1\n"),
		0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+":"+os.Getenv("PATH"))

	// Wire input store so the persist-hook and IPC server share the same store.
	runner := &fakeRunner{store: map[string]string{}}
	is := newFakeInputStore(runner, "fake-storage")
	e.engine.SetInputStore(is)
	e.denoRT.SetInputStore(is)

	// Register a controlled-failure task with on_failure_chain pointing at
	// buildin/auto-fix.
	failing := writeTask(t, dir, "process-payment",
		`export default async () => { throw new Error("boom") }`,
		task.TriggerConfig{Manual: true})
	failing.OnFailureChain = &task.OnFailureChainSpec{
		Task:   "buildin/auto-fix",
		Params: map[string]any{"mode": "review"},
	}
	if err := e.reg.Register(failing); err != nil {
		t.Fatalf("register failing task: %v", err)
	}

	// Register a stub buildin/auto-fix that returns a fake pr_url immediately.
	// A full agent loop is exercised by manual smoke testing, not by this unit test.
	autoFix := writeTask(t, dir, "buildin/auto-fix",
		`export default async () => { return { ok: true, pr_url: "https://github.com/example/repo/pull/1" } }`,
		task.TriggerConfig{Manual: true})
	if err := e.reg.Register(autoFix); err != nil {
		t.Fatalf("register auto-fix stub: %v", err)
	}

	// Fire the failing task and wait for it to reach a terminal state.
	runID, err := e.engine.FireManual(context.Background(), "process-payment", nil)
	if err != nil {
		t.Fatal(err)
	}
	r := waitForTerminal(t, e.engine, runID, 30*time.Second)
	if r.Status != registry.StatusFailure {
		t.Fatalf("primary run status = %q, want failure", r.Status)
	}

	// Wait for the chain dispatcher to fire buildin/auto-fix.
	autoFixRun := waitForRunOfTask(t, e.engine, "buildin/auto-fix", 15*time.Second)
	if autoFixRun == nil {
		t.Fatal("buildin/auto-fix was never fired by the on_failure_chain dispatcher")
	}
	if autoFixRun.Status != registry.StatusSuccess {
		t.Errorf("auto-fix run status = %q, want success", autoFixRun.Status)
	}
	if !strings.Contains(autoFixRun.ReturnValue, "pr_url") {
		t.Errorf("auto-fix did not return pr_url; ReturnValue=%q", autoFixRun.ReturnValue)
	}
}
