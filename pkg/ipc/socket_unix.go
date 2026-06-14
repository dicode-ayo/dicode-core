//go:build !windows

package ipc

import (
	"net"
	"syscall"
)

// listenUnixSecure creates a Unix socket at path with mode 0600 atomically
// by setting the process umask to 0177 for the duration of net.Listen, then
// restoring it. This eliminates the TOCTOU window that exists between a
// plain net.Listen and a subsequent os.Chmod.
//
// Note: syscall.Umask is process-wide and not goroutine-safe. The window
// where the narrowed mask is active is a single OS call, and IPC servers
// are started sequentially (one per task launch), so concurrent Listen calls
// from other goroutines are not expected. The os.Chmod in the caller is
// kept as a belt-and-suspenders fallback.
func listenUnixSecure(socketPath string) (net.Listener, error) {
	old := syscall.Umask(0177) // 0777 &^ 0177 = 0600
	l, err := net.Listen("unix", socketPath)
	syscall.Umask(old)
	return l, err
}
