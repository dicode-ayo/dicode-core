//go:build unix

package runtime

import (
	"os"
	"syscall"
)

// TaskSysProcAttr puts a task subprocess in a process group of its own, so
// everything it spawns can be signalled as one. A Python task runs under `uv`,
// which spawns the interpreter as a child: without the group, a signal reaches
// only the wrapper, and SIGKILL — which no wrapper can forward — would orphan
// the interpreter rather than stop it.
//
// The group also detaches task subprocesses from the terminal's foreground
// group, so a Ctrl-C on a foreground daemon no longer reaches them directly.
// Shutdown does not rely on that: the run context's cancel kills them.
func TaskSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// ProcessGroup returns a Stopper addressing the whole process group led by
// proc. It falls back to proc itself when the group cannot be signalled, which
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
