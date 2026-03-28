// Package deno manages the Deno binary: download, verify, and cache.
package deno

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

	if _, err := os.Stat(cachePath); err == nil {
		return cachePath, nil
	}

	if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}

	platform, err := platformName()
	if err != nil {
		return "", err
	}

	zipURL := fmt.Sprintf(
		"https://github.com/denoland/deno/releases/download/v%s/deno-%s.zip",
		version, platform,
	)
	checksumURL := zipURL + ".sha256sum"

	zipData, err := downloadBytes(zipURL)
	if err != nil {
		return "", fmt.Errorf("download deno: %w", err)
	}

	checksumData, err := downloadBytes(checksumURL)
	if err != nil {
		return "", fmt.Errorf("download checksum: %w", err)
	}

	if err := verifyChecksum(zipData, string(checksumData)); err != nil {
		return "", fmt.Errorf("checksum verification failed: %w", err)
	}

	binName := "deno"
	if runtime.GOOS == "windows" {
		binName = "deno.exe"
	}

	binData, err := extractFromZip(zipData, binName)
	if err != nil {
		return "", fmt.Errorf("extract deno binary: %w", err)
	}

	if err := os.WriteFile(cachePath, binData, 0755); err != nil {
		return "", fmt.Errorf("write deno binary: %w", err)
	}

	return cachePath, nil
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
	binName := "deno"
	if runtime.GOOS == "windows" {
		binName = "deno.exe"
	}
	return filepath.Join(home, ".cache", "dicode", "deno", version, binName), nil
}

func platformName() (string, error) {
	type entry struct{ goos, goarch, name string }
	platforms := []entry{
		{"linux", "amd64", "x86_64-unknown-linux-gnu"},
		{"linux", "arm64", "aarch64-unknown-linux-gnu"},
		{"darwin", "amd64", "x86_64-apple-darwin"},
		{"darwin", "arm64", "aarch64-apple-darwin"},
		{"windows", "amd64", "x86_64-pc-windows-msvc"},
	}
	for _, p := range platforms {
		if p.goos == runtime.GOOS && p.goarch == runtime.GOARCH {
			return p.name, nil
		}
	}
	return "", fmt.Errorf("unsupported platform %s/%s", runtime.GOOS, runtime.GOARCH)
}

func downloadBytes(url string) ([]byte, error) {
	resp, err := http.Get(url) //nolint:gosec
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, url)
	}
	return io.ReadAll(resp.Body)
}

// verifyChecksum checks the SHA-256 of data against a checksum file line
// in the format "<hex>  <filename>" (standard sha256sum output).
func verifyChecksum(data []byte, checksumLine string) error {
	fields := strings.Fields(checksumLine)
	if len(fields) == 0 {
		return fmt.Errorf("empty checksum file")
	}
	expected := strings.ToLower(fields[0])
	h := sha256.Sum256(data)
	got := hex.EncodeToString(h[:])
	if got != expected {
		return fmt.Errorf("expected %s got %s", expected, got)
	}
	return nil
}

func extractFromZip(zipData []byte, name string) ([]byte, error) {
	r, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return nil, err
	}
	for _, f := range r.File {
		if f.Name == name {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("file %q not found in zip", name)
}
