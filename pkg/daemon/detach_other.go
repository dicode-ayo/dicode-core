//go:build !unix

package daemon

import "syscall"

// detachSysProcAttr returns nil: there is no portable session-detach here, so
// the spawned daemon keeps the parent's process group and still dies with the
// console that started it.
func detachSysProcAttr() *syscall.SysProcAttr { return nil }
