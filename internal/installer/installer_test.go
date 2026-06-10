package installer

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestPlatformName(t *testing.T) {
	name, err := PlatformName()
	if err != nil {
		t.Skipf("unsupported platform: %v", err)
	}
	if name == "" {
		t.Error("expected non-empty platform name")
	}
}

func TestVerifyChecksum_Valid(t *testing.T) {
	data := []byte("hello installer")
	h := sha256.Sum256(data)
	line := hex.EncodeToString(h[:]) + "  archive.zip"
	if err := VerifyChecksum(data, line); err != nil {
		t.Errorf("expected valid checksum, got: %v", err)
	}
}

func TestVerifyChecksum_Invalid(t *testing.T) {
	data := []byte("hello installer")
	line := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef  archive.zip"
	if err := VerifyChecksum(data, line); err == nil {
		t.Error("expected checksum mismatch error")
	}
}

func TestVerifyChecksum_Empty(t *testing.T) {
	if err := VerifyChecksum([]byte("data"), ""); err == nil {
		t.Error("expected error for empty checksum line")
	}
}

func TestExtractFromZip_Exact(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("mybinary")
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("fake-binary-content")
	w.Write(want) //nolint:errcheck
	zw.Close()

	got, err := ExtractFromZip(buf.Bytes(), "mybinary", MatchExact)
	if err != nil {
		t.Fatalf("ExtractFromZip: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExtractFromZip_BaseName(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("subdir/mybinary")
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("nested-binary")
	w.Write(want) //nolint:errcheck
	zw.Close()

	got, err := ExtractFromZip(buf.Bytes(), "mybinary", MatchBaseName)
	if err != nil {
		t.Fatalf("ExtractFromZip: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExtractFromZip_Missing(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	zw.Close()

	_, err := ExtractFromZip(buf.Bytes(), "mybinary", MatchExact)
	if err == nil {
		t.Error("expected error for missing file in zip")
	}
}

func TestExtractFromTarGz(t *testing.T) {
	want := []byte("fake-uv-binary")
	data := buildTarGz(t, "uv-platform/uv", want)

	got, err := ExtractFromTarGz(data, "uv")
	if err != nil {
		t.Fatalf("ExtractFromTarGz: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExtractFromTarGz_Missing(t *testing.T) {
	data := buildTarGz(t, "other-file", []byte("content"))

	_, err := ExtractFromTarGz(data, "uv")
	if err == nil {
		t.Error("expected error for missing file in tar.gz")
	}
}

func TestEnsureBinary_Cached(t *testing.T) {
	dir := t.TempDir()
	cachePath := filepath.Join(dir, "mybinary")
	if err := os.WriteFile(cachePath, []byte("cached"), 0755); err != nil {
		t.Fatal(err)
	}

	got, err := EnsureBinary(Spec{CachePath: cachePath})
	if err != nil {
		t.Fatalf("EnsureBinary: %v", err)
	}
	if got != cachePath {
		t.Errorf("expected %s, got %s", cachePath, got)
	}
}

func TestEnsureBinary_DownloadZip(t *testing.T) {
	binContent := []byte("#!/bin/sh\necho hello")
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	w, _ := zw.Create("mybinary")
	w.Write(binContent) //nolint:errcheck
	zw.Close()
	zipData := zipBuf.Bytes()

	h := sha256.Sum256(zipData)
	checksum := hex.EncodeToString(h[:]) + "  archive.zip\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/checksum" {
			w.Write([]byte(checksum)) //nolint:errcheck
		} else {
			w.Write(zipData) //nolint:errcheck
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	cachePath := filepath.Join(dir, "v1", "mybinary")

	got, err := EnsureBinary(Spec{
		ArchiveURL:  srv.URL + "/archive",
		ChecksumURL: srv.URL + "/checksum",
		BinName:     "mybinary",
		CachePath:   cachePath,
		Format:      FormatZip,
		Extract:     MatchExact,
	})
	if err != nil {
		t.Fatalf("EnsureBinary: %v", err)
	}
	if got != cachePath {
		t.Errorf("expected %s, got %s", cachePath, got)
	}

	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(binContent) {
		t.Errorf("binary content mismatch: got %q", data)
	}
}

func TestEnsureBinary_DownloadTarGz(t *testing.T) {
	binContent := []byte("#!/bin/sh\necho uv")
	tarGzData := buildTarGz(t, "uv-platform/uv", binContent)

	h := sha256.Sum256(tarGzData)
	checksum := hex.EncodeToString(h[:]) + "  archive.tar.gz\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/checksum" {
			w.Write([]byte(checksum)) //nolint:errcheck
		} else {
			w.Write(tarGzData) //nolint:errcheck
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	cachePath := filepath.Join(dir, "v1", "uv")

	got, err := EnsureBinary(Spec{
		ArchiveURL:  srv.URL + "/archive",
		ChecksumURL: srv.URL + "/checksum",
		BinName:     "uv",
		CachePath:   cachePath,
		Format:      FormatTarGz,
		Extract:     MatchBaseName,
	})
	if err != nil {
		t.Fatalf("EnsureBinary: %v", err)
	}
	if got != cachePath {
		t.Errorf("expected %s, got %s", cachePath, got)
	}

	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(binContent) {
		t.Errorf("binary content mismatch: got %q", data)
	}
}

func TestEnsureBinary_ChecksumMismatch(t *testing.T) {
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	w, _ := zw.Create("mybinary")
	w.Write([]byte("content")) //nolint:errcheck
	zw.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/checksum" {
			w.Write([]byte("deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef  archive.zip\n")) //nolint:errcheck
		} else {
			w.Write(zipBuf.Bytes()) //nolint:errcheck
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	_, err := EnsureBinary(Spec{
		ArchiveURL:  srv.URL + "/archive",
		ChecksumURL: srv.URL + "/checksum",
		BinName:     "mybinary",
		CachePath:   filepath.Join(dir, "mybinary"),
		Format:      FormatZip,
		Extract:     MatchExact,
	})
	if err == nil {
		t.Error("expected checksum mismatch error")
	}
}

// buildTarGz creates an in-memory .tar.gz with a single file.
func buildTarGz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Size:     int64(len(content)),
		Mode:     0755,
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gw.Close()
	return buf.Bytes()
}
