//go:build !windows

package deno

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/task"
)

// TestRuntime_StaleLockRetryFreshDeadline proves the retry after a stale-lock
// relock runs under a fresh per-attempt timeout budget, not the initial
// attempt's already-spent one. A fake deno stale-fails the first run, the relock
// is stubbed to consume most of the task timeout, and the retry then sleeps long
// enough that it would be killed under a shared budget but completes under a
// fresh one. Uses a fake deno binary + stubbed relock, so it needs no toolchain
// or network.
func TestRuntime_StaleLockRetryFreshDeadline(t *testing.T) {
	e := newTestEnv(t)
	dir := t.TempDir()

	// Fake deno: first invocation emits the stale-lock signature and fails;
	// every later invocation sleeps (the retry) and exits 0. A state file
	// distinguishes the two.
	statePath := filepath.Join(dir, "attempt.state")
	fake := filepath.Join(dir, "fake-deno.sh")
	script := "#!/bin/sh\n" +
		"if [ ! -f '" + statePath + "' ]; then\n" +
		"  : > '" + statePath + "'\n" +
		"  echo 'error: The lockfile is out of date. rerun with --frozen=false' 1>&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"sleep 0.4\n" +
		"exit 0\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e.rt.denoPath = fake

	// deno.lock present + no deno.json ⇒ recovery is enabled for this task.
	if err := os.WriteFile(filepath.Join(dir, "deno.lock"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.ts"),
		[]byte("export default async function main(){ return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.yaml"),
		[]byte("name: fresh-deadline\nruntime: deno\ntrigger:\n  manual: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Relock consumes most of the 1s budget. Under a shared budget the retry
	// would be born with ~0.2s left and its 0.4s subprocess would be killed;
	// a fresh per-attempt budget lets it finish.
	restore := denoRelock
	denoRelock = func(_ context.Context, _ string, _ bool, _ io.Writer) (int, error) {
		time.Sleep(800 * time.Millisecond)
		return 1, nil
	}
	t.Cleanup(func() { denoRelock = restore })

	spec := &task.Spec{
		ID: "fresh-deadline", Name: "fresh-deadline", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true}, Timeout: 1000 * time.Millisecond,
		TaskDir: dir,
	}
	if err := e.reg.Register(spec); err != nil {
		t.Fatal(err)
	}

	r, err := e.rt.Run(context.Background(), spec, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("retry should run under a fresh deadline and succeed, got: %v", r.Error)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("first attempt did not run (no state file): %v", err)
	}
	var sawAudit bool
	for _, l := range r.Logs {
		if strings.Contains(l.Message, "auto-recovery: deno.lock was stale") {
			sawAudit = true
		}
	}
	if !sawAudit {
		t.Errorf("expected auto-recovery audit line, logs: %+v", r.Logs)
	}
}
