//go:build !windows

package python

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
)

// TestExecute_StaleLockRetryFreshDeadline is the Python analogue of the Deno
// fresh-deadline regression test: after a stale-lock relock, the retry must run
// under a fresh per-attempt timeout budget, not the initial attempt's spent one.
// A fake uv stale-fails the first run, the relock is stubbed to consume most of
// the timeout, and the retry then sleeps long enough that it would be killed
// under a shared budget but completes under a fresh one.
func TestExecute_StaleLockRetryFreshDeadline(t *testing.T) {
	rt, reg := newTestRuntime(t)
	dir := t.TempDir()

	// Fake uv: first invocation emits uv's --locked staleness signature and
	// fails; every later invocation sleeps (the retry) and exits 0.
	statePath := filepath.Join(dir, "attempt.state")
	fake := filepath.Join(dir, "fake-uv.sh")
	script := "#!/bin/sh\n" +
		"if [ ! -f '" + statePath + "' ]; then\n" +
		"  : > '" + statePath + "'\n" +
		"  echo 'error: The lockfile needs to be updated, but `--locked` was provided.' 1>&2\n" +
		"  exit 1\n" +
		"fi\n" +
		"sleep 0.4\n" +
		"exit 0\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	scriptPath := filepath.Join(dir, "task.py")
	if err := os.WriteFile(scriptPath, []byte("result = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A sidecar makes the run --locked so the stale path is realistic.
	if err := os.WriteFile(LockSidecarPath(scriptPath), []byte("version = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	restore := pythonRelock
	pythonRelock = func(_ context.Context, _, _ string, _ bool, _ io.Writer) error {
		time.Sleep(800 * time.Millisecond)
		return nil
	}
	t.Cleanup(func() { pythonRelock = restore })

	ex := rt.NewExecutor(fake)
	spec := &task.Spec{
		ID: "fresh-deadline", Name: "fresh-deadline", Runtime: task.Runtime("python"),
		Trigger: task.TriggerConfig{Manual: true}, Timeout: 1000 * time.Millisecond,
		TaskDir: dir,
	}
	if err := reg.Register(spec); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	runID, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	res, err := ex.Execute(ctx, spec, pkgruntime.RunOptions{RunID: runID})
	if err != nil {
		t.Fatalf("Execute transport error: %v", err)
	}
	if res.Error != nil {
		t.Fatalf("retry should run under a fresh deadline and succeed, got: %v", res.Error)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("first attempt did not run (no state file): %v", err)
	}
	logs, err := reg.GetRunLogs(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	var sawAudit bool
	for _, l := range logs {
		if strings.Contains(l.Message, "was stale, regenerated") {
			sawAudit = true
		}
	}
	if !sawAudit {
		t.Errorf("expected auto-recovery audit line, logs: %+v", logs)
	}
}
