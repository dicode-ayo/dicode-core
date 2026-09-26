package ipc

import (
	"net"
	"strconv"
)

// loopbackHost is the only interface a loopback IPC endpoint binds to. It is
// also the host half of the address handed to the task as DICODE_SOCKET, so
// IsLoopbackAddr and the sandbox grants derived from it stay in agreement.
const loopbackHost = "127.0.0.1"

// IsLoopbackAddr reports whether addr — the per-run endpoint address returned
// by Server.Start and handed to the task as DICODE_SOCKET — is a loopback TCP
// endpoint rather than a Unix-socket path.
//
// Runtimes branch on it to reach the endpoint the way their sandbox allows:
// a loopback endpoint needs a network grant scoped to this host:port, a Unix
// socket needs read+write on the socket file.
func IsLoopbackAddr(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host != loopbackHost {
		return false
	}
	_, err = strconv.Atoi(port)
	return err == nil
}
