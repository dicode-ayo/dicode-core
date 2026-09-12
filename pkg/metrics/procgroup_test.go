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

// TestProcessGroupMembers_LeaderOnly: this process shares its group with the
// rest of the test binary's session, so matching on group id alone would
// gather every one of them. Only a PID that actually leads a group has
// members, and this one does not.
func TestProcessGroupMembers_LeaderOnly(t *testing.T) {
	self := os.Getpid()
	got := processGroupMembers([]int{self})
	if len(got[self]) != 1 || got[self][0] != self {
		t.Errorf("processGroupMembers([%d]) = %v; want just the pid itself", self, got[self])
	}
}

// TestProcessGroupMembers_GathersOneScanPerCall: every tracked PID's group has
// to come out of the single /proc walk, not just the first.
func TestProcessGroupMembers_GathersEveryGroup(t *testing.T) {
	var leaders []int
	for i := 0; i < 2; i++ {
		cmd := exec.Command("/bin/sh", "-c", `sh -c 'while :; do sleep 0.05; done' & wait`)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatalf("start: %v", err)
		}
		pid := cmd.Process.Pid
		defer func() {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_, _ = cmd.Process.Wait()
		}()
		leaders = append(leaders, pid)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got := processGroupMembers(leaders)
		if len(got[leaders[0]]) > 1 && len(got[leaders[1]]) > 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := processGroupMembers(leaders)
	t.Errorf("processGroupMembers(%v) = %v; want both groups to carry their child", leaders, got)
}
