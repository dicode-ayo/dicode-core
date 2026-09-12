//go:build !unix

package runtime

import (
	"os"
	"syscall"
)

// TaskSysProcAttr returns nil: there is no portable process-group placement
// here, so a task subprocess keeps the parent's group.
func TaskSysProcAttr() *syscall.SysProcAttr { return nil }

// ProcessGroup returns proc unchanged: without a group of its own there is
// nothing wider to address.
func ProcessGroup(proc *os.Process) Stopper { return proc }
