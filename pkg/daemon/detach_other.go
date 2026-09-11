//go:build !unix

package daemon

import "syscall"

// detachSysProcAttr returns nil: there is no portable session-detach here, so
// the spawned daemon keeps the parent's process group.
func detachSysProcAttr() *syscall.SysProcAttr { return nil }
