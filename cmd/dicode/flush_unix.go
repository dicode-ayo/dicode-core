//go:build unix

package main

import "golang.org/x/sys/unix"

// flushStdinFD discards input the kernel's line discipline has buffered for
// fd (TCIFLUSH via TCFLSH), so keystrokes typed during a blocking daemon call
// are not fed to the next prompt.
func flushStdinFD(fd int) {
	_ = unix.IoctlSetPointerInt(fd, unix.TCFLSH, unix.TCIFLUSH)
}
