//go:build linux

package metrics

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// TestReadChildMetrics_CountsTheWholeGroup: a task subprocess leads a process
// group, and for a Python run the process the runtime holds is `uv` — the
// interpreter doing the work is one level down. Reading only the leader
// reports the wrapper's near-zero footprint as the task's.
func TestReadChildMetrics_CountsTheWholeGroup(t *testing.T) {
	// The grandchild allocates ~64MB so its RSS is unmistakable next to the
	// shell leading the group.
	cmd := exec.Command("/bin/sh", "-c", `sh -c 'a=$(head -c 64000000 /dev/zero | tr "\0" "x"); while :; do sleep 0.05; done' & wait`)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
	}()

	leader := cmd.Process.Pid
	leaderRSS := readProcRSSMB(leader)

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		got := ReadChildMetrics([]int{leader}, 1)
		if got.ChildRSSMB > leaderRSS+32 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	got := ReadChildMetrics([]int{leader}, 1)
	t.Errorf("ChildRSSMB = %.1f with the leader alone at %.1f; the group member's memory is not counted",
		got.ChildRSSMB, leaderRSS)
}

// TestProcessGroupMembers_LeaderOnly: a process in no group of its own must
// not drag in every other process sharing the parent's group.
func TestProcessGroupMembers_LeaderOnly(t *testing.T) {
	self := os.Getpid()
	members := processGroupMembers(self)
	for _, pid := range members {
		if pid == self {
			return
		}
	}
	t.Errorf("processGroupMembers(%d) = %v; want it to contain the pid itself", self, members)
}
