//go:build windows

package ipc

import "net"

// listenUnixSecure on Windows falls back to a plain net.Listen. Unix socket
// permissions on Windows are governed by ACLs rather than mode bits.
func listenUnixSecure(socketPath string) (net.Listener, error) {
	return net.Listen("unix", socketPath)
}
