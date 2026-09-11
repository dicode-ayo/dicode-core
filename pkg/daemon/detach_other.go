//go:build !unix

package daemon

import (
	"os"
	"syscall"

	"golang.org/x/term"
)

// detachSysProcAttr returns nil: there is no portable session-detach here, so
// the spawned daemon keeps the parent's process group and still dies with the
// console that started it.
func detachSysProcAttr() *syscall.SysProcAttr { return nil }

// hasControllingTerminal falls back to the standard streams without a /dev/tty
// to open. Any one of them still pointing at a console means a console is
// attached.
func hasControllingTerminal() bool {
	for _, f := range []*os.File{os.Stdin, os.Stdout, os.Stderr} {
		if term.IsTerminal(int(f.Fd())) {
			return true
		}
	}
	return false
}

// ProcessAlive always reports true without a signal-0 probe to run, leaving
// the control socket as the only evidence a caller can wait on.
func ProcessAlive(int) bool { return true }
