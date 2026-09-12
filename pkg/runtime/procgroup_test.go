//go:build unix

package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// startGroupLeader runs a shell that spawns a grandchild and waits, mirroring
// the shape of `uv run python`: the process the runtime holds is a wrapper,
// and the work happens one level down.
func startGroupLeader(t *testing.T) (*exec.Cmd, int) {
	t.Helper()

	// The child writes its own PID to a file so the test can watch it
	// independently of the wrapper.
	pidFile := t.TempDir() + "/child.pid"
	cmd := exec.Command("/bin/sh", "-c", `sh -c 'echo $$ > "$0"; while :; do sleep 0.05; done' "$1" & wait`, pidFile, pidFile)
	cmd.SysProcAttr = TaskSysProcAttr()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(pidFile); err == nil && len(b) > 1 {
			var pid int
			if _, err := fmtSscan(string(b), &pid); err == nil && pid > 0 {
				return cmd, pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("grandchild never reported its PID")
	return nil, 0
}

// TestProcessGroup_KillReachesTheGrandchild: a Python task runs under `uv`, so
// the process the runtime holds is a wrapper. A signal sent to the wrapper
// alone leaves the interpreter running — and SIGKILL cannot be forwarded at
// all, so the escalation would orphan it rather than stop it.
func TestProcessGroup_KillReachesTheGrandchild(t *testing.T) {
	cmd, grandchild := startGroupLeader(t)

	if !processAlive(grandchild) {
		t.Fatal("grandchild not running at the start of the test")
	}

	if err := ProcessGroup(cmd.Process).Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	_, _ = cmd.Process.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(grandchild) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("grandchild %d survived the group kill", grandchild)
}

// TestProcessGroup_SignalReachesTheGrandchild is the SIGTERM half: the polite
// signal has to reach the interpreter too, or the graceful stop never happens
// and every hung run costs the full kill grace.
func TestProcessGroup_SignalReachesTheGrandchild(t *testing.T) {
	cmd, grandchild := startGroupLeader(t)

	if err := ProcessGroup(cmd.Process).Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	_, _ = cmd.Process.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(grandchild) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("grandchild %d survived the group SIGTERM", grandchild)
}

// processAlive reports whether pid still names a live process. Signal 0 tests
// for existence without delivering anything.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// fmtSscan wraps fmt.Sscan so the import stays local to its one use.
func fmtSscan(s string, a ...any) (int, error) { return fmt.Sscan(s, a...) }
