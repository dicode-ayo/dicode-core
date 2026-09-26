//go:build !windows

package ipc

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// listenIPC creates the per-run endpoint: a Unix socket inside a per-run
// directory created 0700 (/tmp/dicode-<runID>/ipc.sock). The directory makes
// the socket unreachable by other local users independent of the socket file's
// own mode and the umask at creation time.
//
// dir is the directory the caller must remove when the run ends.
func listenIPC(runID string) (l net.Listener, addr, dir string, err error) {
	dir = filepath.Join("/tmp", "dicode-"+runID)
	// Remove any leftover dir from a previous (crashed) run.
	_ = os.RemoveAll(dir)
	if err := os.Mkdir(dir, 0700); err != nil {
		return nil, "", "", fmt.Errorf("ipc: mkdir socket dir: %w", err)
	}
	addr = filepath.Join(dir, "ipc.sock")

	l, err = net.Listen("unix", addr)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, "", "", fmt.Errorf("ipc: listen %s: %w", addr, err)
	}
	// Belt-and-suspenders on top of the 0700 parent: it also closes the brief
	// pre-chmod window on platforms that lack sticky-bit /tmp behavior.
	if err := os.Chmod(addr, 0600); err != nil {
		_ = l.Close()
		_ = os.RemoveAll(dir)
		return nil, "", "", fmt.Errorf("ipc: chmod socket: %w", err)
	}
	return l, addr, dir, nil
}
