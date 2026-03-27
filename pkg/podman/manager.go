// Package podman locates the system podman binary.
// Unlike Deno and uv, podman is not downloaded by dicode — it is expected to
// be installed via the system package manager (dnf, apt, brew, …).
package podman

import (
	"fmt"
	"os/exec"
	"strings"
)

// BinaryPath returns the absolute path to the podman binary.
// It searches PATH and the common extra location /usr/bin/podman.
func BinaryPath() (string, error) {
	p, err := exec.LookPath("podman")
	if err != nil {
		return "", fmt.Errorf("podman not found in PATH — install it via your system package manager (e.g. dnf install podman, apt install podman, brew install podman)")
	}
	return p, nil
}

// IsInstalled reports whether podman is available on this system.
func IsInstalled() bool {
	_, err := BinaryPath()
	return err == nil
}

// Version returns the short version string reported by `podman --version`,
// e.g. "5.4.2". Returns "unknown" if the binary cannot be queried.
func Version() string {
	p, err := BinaryPath()
	if err != nil {
		return "unknown"
	}
	out, err := exec.Command(p, "--version").Output() //nolint:gosec
	if err != nil {
		return "unknown"
	}
	// "podman version 5.4.2\n"
	parts := strings.Fields(strings.TrimSpace(string(out)))
	if len(parts) >= 3 {
		return parts[2]
	}
	return strings.TrimSpace(string(out))
}
