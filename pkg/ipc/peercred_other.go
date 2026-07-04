//go:build !linux

package ipc

import (
	"net"
	"os"
	"path/filepath"

	"github.com/dicode/dicode/internal/fsutil"
)

const peerCredSupported = false

// peerUIDMatches always returns false on non-Linux platforms; callers fall
// back to token-file authentication.
func peerUIDMatches(_ net.Conn) (bool, error) { return false, nil }

// writeCLITokenFile writes the CLI token to path atomically (tmp + rename,
// mode 0600). Only used on non-Linux platforms — on Linux the CLI control
// channel authenticates via SO_PEERCRED and no token file is written.
func writeCLITokenFile(path, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, []byte(token), 0600)
}

// readCLITokenFile reads the token from disk on non-Linux platforms.
func readCLITokenFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
