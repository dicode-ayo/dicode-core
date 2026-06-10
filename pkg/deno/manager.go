// Package deno manages the Deno binary: download, verify, and cache.
package deno

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/dicode/dicode/internal/installer"
)

// EnsureDeno returns the path to the cached Deno binary for the current
// platform, downloading and verifying it first if necessary.
func EnsureDeno(version string) (string, error) {
	if version == "" {
		version = DefaultVersion
	}

	cachePath, err := cacheBinPath(version)
	if err != nil {
		return "", err
	}

	platform, err := installer.PlatformName()
	if err != nil {
		return "", err
	}

	zipURL := fmt.Sprintf(
		"https://github.com/denoland/deno/releases/download/v%s/deno-%s.zip",
		version, platform,
	)

	return installer.EnsureBinary(installer.Spec{
		ArchiveURL:  zipURL,
		ChecksumURL: zipURL + ".sha256sum",
		BinName:     binName(),
		CachePath:   cachePath,
		Format:      installer.FormatZip,
		Extract:     installer.MatchExact,
	})
}

// BinaryPath returns the expected filesystem path for the cached Deno binary
// at the given version, regardless of whether it is installed.
func BinaryPath(version string) (string, error) {
	return cacheBinPath(version)
}

func cacheBinPath(version string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".cache", "dicode", "deno", version, binName()), nil
}

func binName() string {
	if runtime.GOOS == "windows" {
		return "deno.exe"
	}
	return "deno"
}
