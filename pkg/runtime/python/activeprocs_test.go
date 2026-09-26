//go:build !windows

package python

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/metrics"
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

// TestExecute_MetricsSeeTheInterpreter: a Python task runs as `uv run python`,
// so the tracked PID leads a group whose second member is the interpreter
// holding the task's memory. Reading the leader alone reports uv's footprint
// as the task's.
func TestExecute_MetricsSeeTheInterpreter(t *testing.T) {
	if testing.Short() {
		t.Skip("requires uv subprocess")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	reg, ex := newSuspendExecutor(t)

	spec := writePythonTask(t, "examples/hungry", `
import time

async def main():
    blob = bytearray(64 * 1024 * 1024)
    blob[0] = 1
    time.sleep(3)
    return len(blob)
`)
	if err := reg.Register(spec); err != nil {
		t.Fatal(err)
	}
	runID, err := reg.StartRun(ctx, spec.ID, "")
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	peak := make(chan float64, 1)
	go func() {
		defer wg.Done()
		var best float64
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			pids := pkgruntime.ActivePIDs()
			if len(pids) == 0 && best > 0 {
				break
			}
			if rss := metrics.ReadChildMetrics(pids, len(pids)).ChildRSSMB; rss > best {
				best = rss
			}
			time.Sleep(20 * time.Millisecond)
		}
		peak <- best
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

	// The task holds 64MB; uv alone is an order of magnitude smaller.
	if got := <-peak; got < 32 {
		t.Errorf("peak ChildRSSMB = %.1f; the interpreter's 64MB never reached the metrics", got)
	}
}
