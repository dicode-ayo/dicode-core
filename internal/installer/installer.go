// Package installer provides shared logic for downloading, verifying, and
// caching pinned binary releases from GitHub. It is used by the deno and uv
// packages to avoid duplicating the download/verify/extract/cache pipeline.
package installer

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

// ArchiveFormat describes the archive type to download and extract.
type ArchiveFormat int

const (
	// FormatZip is a .zip archive (used by deno on all platforms, uv on Windows).
	FormatZip ArchiveFormat = iota
	// FormatTarGz is a .tar.gz archive (used by uv on non-Windows).
	FormatTarGz
)

// ExtractMode controls how the binary is located inside the archive.
type ExtractMode int

const (
	// MatchExact requires the archive entry name to equal the binary name exactly.
	MatchExact ExtractMode = iota
	// MatchBaseName matches on filepath.Base(entry) == binary name,
	// allowing the binary to be inside a subdirectory.
	MatchBaseName
)

// Spec describes everything needed to download, verify, and cache a binary.
type Spec struct {
	// ArchiveURL is the URL of the archive to download.
	ArchiveURL string
	// ChecksumURL is the URL of the SHA-256 checksum file.
	ChecksumURL string
	// BinName is the name of the binary to extract (e.g. "deno", "uv.exe").
	BinName string
	// CachePath is the final filesystem path where the binary will be stored.
	CachePath string
	// CacheBase is the root directory that CachePath must reside under.
	// Used to prevent path-traversal attacks. Defaults to ~/.cache/dicode
	// when empty.
	CacheBase string
	// Format is the archive format (zip or tar.gz).
	Format ArchiveFormat
	// Extract controls how the binary is located in the archive.
	Extract ExtractMode
}

// EnsureBinary downloads, verifies, extracts, and caches a binary according
// to the given Spec. If the binary already exists at CachePath it is returned
// immediately without any network access.
func EnsureBinary(s Spec) (string, error) {
	// Validate CachePath against traversal. Callers (pkg/deno, pkg/uv) build
	// it from hardcoded segments + a version string; Clean+HasPrefix is
	// defence-in-depth so a rogue version can't escape the cache tree.
	cachePath := filepath.Clean(s.CachePath)
	cacheBase := s.CacheBase
	if cacheBase == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		cacheBase = filepath.Join(home, ".cache", "dicode")
	}
	if !strings.HasPrefix(cachePath, filepath.Clean(cacheBase)+string(filepath.Separator)) {
		return "", fmt.Errorf("cache path %q escapes base %q", cachePath, cacheBase)
	}

	if _, err := os.Stat(cachePath); err == nil {
		return cachePath, nil
	}

	if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}

	archiveData, err := DownloadBytes(s.ArchiveURL)
	if err != nil {
		return "", fmt.Errorf("download archive: %w", err)
	}

	checksumData, err := DownloadBytes(s.ChecksumURL)
	if err != nil {
		return "", fmt.Errorf("download checksum: %w", err)
	}

	if err := VerifyChecksum(archiveData, string(checksumData)); err != nil {
		return "", fmt.Errorf("checksum verification failed: %w", err)
	}

	var binData []byte
	switch s.Format {
	case FormatZip:
		binData, err = ExtractFromZip(archiveData, s.BinName, s.Extract)
	case FormatTarGz:
		binData, err = ExtractFromTarGz(archiveData, s.BinName)
	default:
		return "", fmt.Errorf("unsupported archive format: %d", s.Format)
	}
	if err != nil {
		return "", fmt.Errorf("extract binary: %w", err)
	}

	// Write to a temp file in the same directory, then rename atomically.
	// This prevents concurrent downloaders from corrupting the binary.
	tmp, err := os.CreateTemp(filepath.Dir(cachePath), "installer-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(binData); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("write binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0755); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("chmod binary: %w", err)
	}
	if err := os.Rename(tmpPath, cachePath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("install binary: %w", err)
	}

	return cachePath, nil
}

// PlatformName returns the canonical platform triple for the current OS/arch
// combination (e.g. "x86_64-unknown-linux-gnu"). Both deno and uv use the
// same set of platform strings.
func PlatformName() (string, error) {
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

// DownloadBytes fetches the contents of the given URL into memory.
func DownloadBytes(url string) ([]byte, error) {
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

// VerifyChecksum checks the SHA-256 of data against a checksum file line
// in the format "<hex>  <filename>" (standard sha256sum output).
func VerifyChecksum(data []byte, checksumLine string) error {
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

// ExtractFromZip extracts a file from a zip archive. When mode is MatchExact,
// the entry name must equal binName exactly. When mode is MatchBaseName,
// filepath.Base(entry) is compared instead.
func ExtractFromZip(zipData []byte, binName string, mode ExtractMode) ([]byte, error) {
	r, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return nil, err
	}
	for _, f := range r.File {
		match := false
		switch mode {
		case MatchExact:
			match = f.Name == binName
		case MatchBaseName:
			match = filepath.Base(f.Name) == binName
		}
		if match {
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

// ExtractFromTarGz finds the first entry whose base name matches binName
// inside a .tar.gz archive and returns its content.
func ExtractFromTarGz(data []byte, binName string) ([]byte, error) {
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
