package uv

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureUv_CachedPath(t *testing.T) {
	// Write a fake binary to the cache path, then verify EnsureUv returns it
	// without hitting the network.
	cachePath, err := BinaryPath("0.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cachePath, []byte("fake"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(cachePath) })

	got, err := EnsureUv("0.0.0-test")
	if err != nil {
		t.Fatalf("EnsureUv: %v", err)
	}
	if got != cachePath {
		t.Errorf("expected %s, got %s", cachePath, got)
	}
}

func TestBinaryPath(t *testing.T) {
	p, err := BinaryPath("1.2.3")
	if err != nil {
		t.Fatal(err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(home, ".cache", "dicode", "uv", "1.2.3", binName())
	if p != want {
		t.Errorf("BinaryPath = %q, want %q", p, want)
	}
}
