//go:build unix

package daemon

import (
	"os"
	"syscall"
)

// detachSysProcAttr puts the spawned daemon in a session of its own, which is
// what severs it from the controlling terminal.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// hasControllingTerminal reports whether a terminal can take this process
// down. Opening /dev/tty succeeds only for a process that has one, and is
// unaffected by where the standard streams point: `dicode daemon < /dev/null`
// still dies with its terminal, so testing os.Stdin would call such a daemon
// backgrounded and leave it undetachable.
func hasControllingTerminal() bool {
	tty, err := os.OpenFile("/dev/tty", os.O_RDONLY, 0)
	if err != nil {
		return false
	}
	tty.Close()
	return true
}

// ProcessAlive reports whether pid names a live process. Signal 0 runs the
// kernel's existence and permission checks without delivering anything;
// EPERM means the process is there but owned by somebody else.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
