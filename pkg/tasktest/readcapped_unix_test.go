//go:build unix

package tasktest

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReadCapped_FIFOSwappedAfterLstat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Dockerfile")
	if err := os.WriteFile(path, []byte("FROM alpine:3.21 AS test\nFROM alpine:3.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := readCapped(path, fi, maxDockerfileBytes)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("readCapped accepted a FIFO swapped in after Lstat")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readCapped blocked opening a FIFO")
	}
}
