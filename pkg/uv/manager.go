// Package uv manages the uv binary: download, verify, and cache.
// uv is the fast Python package manager and script runner used by dicode's
// Python runtime (https://github.com/astral-sh/uv).
package uv

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/dicode/dicode/internal/installer"
)

// EnsureUv returns the path to the cached uv binary for the current
// platform, downloading and verifying it first if necessary.
func EnsureUv(version string) (string, error) {
	if version == "" {
		version = DefaultVersion
	}

	cachePath, err := BinaryPath(version)
	if err != nil {
		return "", err
	}

	platform, err := installer.PlatformName()
	if err != nil {
		return "", err
	}

	// uv uses .zip on Windows, .tar.gz everywhere else.
	var (
		archiveExt string
		format     installer.ArchiveFormat
	)
	if runtime.GOOS == "windows" {
		archiveExt = ".zip"
		format = installer.FormatZip
	} else {
		archiveExt = ".tar.gz"
		format = installer.FormatTarGz
	}

	archiveName := fmt.Sprintf("uv-%s%s", platform, archiveExt)
	archiveURL := fmt.Sprintf(
		"https://github.com/astral-sh/uv/releases/download/%s/%s",
		version, archiveName,
	)

	return installer.EnsureBinary(installer.Spec{
		ArchiveURL:  archiveURL,
		ChecksumURL: archiveURL + ".sha256",
		BinName:     binName(),
		CachePath:   cachePath,
		Format:      format,
		Extract:     installer.MatchBaseName,
	})
}

// BinaryPath returns the expected filesystem path for the cached uv binary at
// the given version, regardless of whether it is installed.
func BinaryPath(version string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".cache", "dicode", "uv", version, binName()), nil
}

func binName() string {
	if runtime.GOOS == "windows" {
		return "uv.exe"
	}
	return "uv"
}
