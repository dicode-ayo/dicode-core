// Package uv manages the uv binary: download, verify, and cache.
// uv is the fast Python package manager and script runner used by dicode's
// Python runtime (https://github.com/astral-sh/uv).
package uv

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
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

	// uv uses .zip on Windows, .tar.gz everywhere else.
	var archiveExt string
	if runtime.GOOS == "windows" {
		archiveExt = ".zip"
	} else {
		archiveExt = ".tar.gz"
	}
	archiveName := fmt.Sprintf("uv-%s%s", platform, archiveExt)
	archiveURL := fmt.Sprintf(
		"https://github.com/astral-sh/uv/releases/download/%s/%s",
		version, archiveName,
	)
	checksumURL := archiveURL + ".sha256"

	archiveData, err := downloadBytes(archiveURL)
	if err != nil {
		return "", fmt.Errorf("download uv: %w", err)
	}

	checksumData, err := downloadBytes(checksumURL)
	if err != nil {
		return "", fmt.Errorf("download checksum: %w", err)
	}

	if err := verifyChecksum(archiveData, string(checksumData)); err != nil {
		return "", fmt.Errorf("checksum verification failed: %w", err)
	}

	binName := "uv"
	if runtime.GOOS == "windows" {
		binName = "uv.exe"
	}

	var binData []byte
	if runtime.GOOS == "windows" {
		binData, err = extractFromZip(archiveData, binName)
	} else {
		binData, err = extractFromTarGz(archiveData, binName)
	}
	if err != nil {
		return "", fmt.Errorf("extract uv binary: %w", err)
	}

	if err := os.WriteFile(cachePath, binData, 0755); err != nil {
		return "", fmt.Errorf("write uv binary: %w", err)
	}

	return cachePath, nil
}

// BinaryPath returns the expected filesystem path for the cached uv binary at
// the given version, regardless of whether it is installed.
func BinaryPath(version string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	binName := "uv"
	if runtime.GOOS == "windows" {
		binName = "uv.exe"
	}
	return filepath.Join(home, ".cache", "dicode", "uv", version, binName), nil
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

// extractFromTarGz finds the first entry whose base name matches binName
// inside a .tar.gz archive and returns its content.
// uv archives contain entries like "uv-x86_64-unknown-linux-gnu/uv".
func extractFromTarGz(data []byte, binName string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("open gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if filepath.Base(hdr.Name) == binName && hdr.Typeflag == tar.TypeReg {
			return io.ReadAll(tr)
		}
	}
	return nil, fmt.Errorf("file %q not found in tar archive", binName)
}

// extractFromZip finds the entry whose base name matches binName in a .zip
// archive (used on Windows).
func extractFromZip(data []byte, binName string) ([]byte, error) {
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	for _, f := range r.File {
		if filepath.Base(f.Name) == binName {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("file %q not found in zip", binName)
}
