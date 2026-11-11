//go:build unix

package task

import (
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestHash_IncludeFifoIsSkippedNotRead: a hash_include entry naming a FIFO
// must be skipped, not read — os.ReadFile blocks forever on a FIFO with no
// writer.
func TestHash_IncludeFifoIsSkippedNotRead(t *testing.T) {
	tasksRoot := t.TempDir()
	fifoPath := filepath.Join(tasksRoot, "a.fifo")
	if err := unix.Mkfifo(fifoPath, 0644); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}

	dir := filepath.Join(tasksRoot, "task-a")
	writeTaskFiles(t, dir, "name: a\n", "js")

	done := make(chan struct{})
	var hash string
	var hashErr error
	go func() {
		hash, hashErr = Hash(dir, "../a.fifo")
		close(done)
	}()
	select {
	case <-done:
		if hashErr != nil {
			t.Fatalf("Hash errored on a FIFO include: %v", hashErr)
		}
		if hash == "" {
			t.Fatal("Hash returned an empty digest")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Hash blocked reading a FIFO hash_include entry instead of skipping it")
	}
}

// TestHash_InDirFifoIsSkipped: a FIFO directly in the walked dir is skipped.
// walkTree classifies entries by a real stat (d.Info()) because d.Type() can
// report 0 on filesystems without d_type.
func TestHash_InDirFifoIsSkipped(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "task-a")
	writeTaskFiles(t, dir, "name: a\n", "js")
	fifoPath := filepath.Join(dir, "a.fifo")
	if err := unix.Mkfifo(fifoPath, 0644); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}

	done := make(chan struct{})
	var hash string
	var hashErr error
	go func() {
		hash, hashErr = Hash(dir)
		close(done)
	}()
	select {
	case <-done:
		if hashErr != nil {
			t.Fatalf("Hash errored on an in-dir FIFO: %v", hashErr)
		}
		if hash == "" {
			t.Fatal("Hash returned an empty digest")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Hash blocked reading an in-dir FIFO instead of skipping it")
	}
}
