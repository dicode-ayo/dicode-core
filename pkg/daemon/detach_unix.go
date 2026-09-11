//go:build unix

package daemon

import "syscall"

// detachSysProcAttr puts the spawned daemon in a session of its own, which is
// what severs it from the controlling terminal.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
