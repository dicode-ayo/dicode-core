//go:build unix

package runtime

import (
	"os"
	"os/exec"
	"syscall"
)

// ConfigureTaskProcess prepares a task subprocess so that everything it
// spawns can be stopped as one. cmd must come from exec.CommandContext: the
// context's cancel is what enforces a task's timeout and what shutdown leans
// on, and the default cancel kills the leader alone — for a Python task that
// is `uv`, not the interpreter under it.
//
// WaitDelay bounds the wait afterwards. A group member that escaped with
// setsid() still holds the write end of the run's stderr pipe, and cmd.Wait
// blocks on the copier until every writer closes it.
func ConfigureTaskProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = TaskSysProcAttr()
	cmd.Cancel = func() error { return ProcessGroup(cmd.Process).Kill() }
	cmd.WaitDelay = BridgeKillGrace
}

// TaskSysProcAttr puts a task subprocess in a process group of its own, so
// everything it spawns can be signaled as one. A Python task runs under `uv`,
// which spawns the interpreter as a child: without the group, a signal reaches
// only the wrapper, and SIGKILL — which no wrapper can forward — would orphan
// the interpreter rather than stop it.
//
// The group also detaches task subprocesses from the terminal's foreground
// group, so a Ctrl-C on a foreground daemon no longer reaches them directly.
// ConfigureTaskProcess is what covers that: the run context's cancel stops the
// group.
func TaskSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// ProcessGroup returns a Stopper addressing the whole process group led by
// proc. It falls back to proc itself when the group cannot be signaled, which
// is what happens if the process was started without TaskSysProcAttr.
func ProcessGroup(proc *os.Process) Stopper {
	return processGroup{proc: proc}
}

type processGroup struct{ proc *os.Process }

func (g processGroup) Signal(sig os.Signal) error {
	s, ok := sig.(syscall.Signal)
	if !ok {
		return g.proc.Signal(sig)
	}
	if err := syscall.Kill(-g.proc.Pid, s); err != nil {
		return g.proc.Signal(sig)
	}
	return nil
}

func (g processGroup) Kill() error {
	if err := syscall.Kill(-g.proc.Pid, syscall.SIGKILL); err != nil {
		return g.proc.Kill()
	}
	return nil
}
