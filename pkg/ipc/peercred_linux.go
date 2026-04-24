//go:build linux

package ipc

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// peerCredSupported reports whether this platform authenticates CLI control
// connections via SO_PEERCRED UID match. When true, the token-file path is
// not written or read for the CLI control channel.
const peerCredSupported = true

// peerUIDMatches reports whether the peer on conn has the same UID as the
// current process. The Linux kernel fills ucred at connect() time, so this
// is race-safe — there is no TOCTOU window between accept and the check.
// Returns (false, nil) for non-AF_UNIX conns.
func peerUIDMatches(conn net.Conn) (bool, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return false, nil
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return false, fmt.Errorf("raw conn: %w", err)
	}
	var ucred *unix.Ucred
	var sockErr error
	ctlErr := raw.Control(func(fd uintptr) {
		ucred, sockErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if ctlErr != nil {
		return false, fmt.Errorf("control fd: %w", ctlErr)
	}
	if sockErr != nil {
		return false, fmt.Errorf("SO_PEERCRED: %w", sockErr)
	}
	return int(ucred.Uid) == os.Getuid(), nil
}

// writeCLITokenFile is a no-op on Linux — the CLI authenticates via
// SO_PEERCRED, so there is no token file to leave on disk.
func writeCLITokenFile(_, _ string) error { return nil }

// readCLITokenFile returns the empty string on Linux. Dial passes "" to the
// server, which accepts it when peerUIDMatches succeeds.
func readCLITokenFile(_ string) (string, error) { return "", nil }
