//go:build windows

package ipc

import (
	"fmt"
	"net"
)

// listenIPC creates the per-run endpoint on an ephemeral loopback TCP port.
//
// Windows has no transport both task SDKs can speak: Deno has no Unix-socket
// support there (denoland/deno#18236) and asyncio.open_unix_connection is
// documented Unix-only. Named pipes are reachable from Deno only under
// --allow-all, which would disable the sandbox the Deno runtime exists to
// provide.
//
// The listener binds 127.0.0.1, so the endpoint is unreachable off-host, but
// any local process can connect to the port: unlike the Unix socket in a 0700
// directory, the run-scoped handshake token is the only access control here.
//
// dir is always empty — there is no directory to clean up.
func listenIPC(_ string) (l net.Listener, addr, dir string, err error) {
	l, err = net.Listen("tcp", net.JoinHostPort(loopbackHost, "0"))
	if err != nil {
		return nil, "", "", fmt.Errorf("ipc: listen loopback: %w", err)
	}
	return l, l.Addr().String(), "", nil
}
