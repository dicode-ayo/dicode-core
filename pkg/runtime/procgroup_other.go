//go:build !unix

package runtime

import (
	"os"
	"os/exec"
	"syscall"
)

// ConfigureTaskProcess bounds the wait after the context's cancel. Without a
// process group there is nothing wider for the cancel itself to address.
func ConfigureTaskProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = TaskSysProcAttr()
	cmd.WaitDelay = BridgeKillGrace
}

// TaskSysProcAttr returns nil: there is no portable process-group placement
// here, so a task subprocess keeps the parent's group.
func TaskSysProcAttr() *syscall.SysProcAttr { return nil }

// ProcessGroup returns proc unchanged: without a group of its own there is
// nothing wider to address.
func ProcessGroup(proc *os.Process) Stopper { return proc }
