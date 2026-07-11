//go:build !linux

package main

// flushStdinFD is a no-op on platforms without a termios TCFLSH ioctl
// (macOS, Windows, BSD — TCFLSH is Linux-only).
func flushStdinFD(fd int) {}
