//go:build !unix

package main

// flushStdinFD is a no-op on platforms without a termios TCFLSH ioctl
// (Windows and others).
func flushStdinFD(fd int) {}
