//go:build !windows

package python

import (
	"context"
	"sync"
	"testing"
	"time"

	pkgruntime "github.com/dicode/dicode/pkg/runtime"
)

// TestExecute_SubprocessIsTracked: the per-child resource metrics and the
// daemon's shutdown sweep both work off the shared PID set, so a Python run
// has to appear in it while it is running and be gone once it returns.
func TestExecute_SubprocessIsTracked(t *testing.T) {
	if testing.Short() {
		t.Skip("requires uv subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	reg, ex := newSuspendExecutor(t)

	spec := writePythonTask(t, "examples/tracked", `
import time

async def main():
    time.sleep(2)
    return "done"
`)
	if err := reg.Register(spec); err != nil {
		t.Fatal(err)
	}
	runID, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	before := len(pkgruntime.ActivePIDs())

	var wg sync.WaitGroup
	wg.Add(1)
	seen := make(chan int, 1)
	go func() {
		defer wg.Done()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if n := len(pkgruntime.ActivePIDs()); n > before {
				seen <- n
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()

	res, err := ex.Execute(ctx, spec, pkgruntime.RunOptions{RunID: runID})
	wg.Wait()
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Error != nil {
		dumpRunLogs(t, reg, runID)
		t.Fatalf("run failed: %v", res.Error)
	}

	select {
	case <-seen:
	default:
		t.Error("the Python subprocess never appeared in ActivePIDs")
	}

	if n := len(pkgruntime.ActivePIDs()); n > before {
		t.Errorf("ActivePIDs() left %d extra entries after the run returned", n-before)
	}
}
