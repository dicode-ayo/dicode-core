//go:build unix

package task

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestHash_IncludeFifoIsSkippedNotRead is the regression for a hang caught
// in review: a hash_include entry naming a non-regular, non-symlink,
// non-directory path (a FIFO here) must be silently skipped, exactly like
// walkTree already does for the same file types found during a normal dir
// walk — not read via os.ReadFile, which blocks indefinitely on a FIFO with
// no writer.
func TestHash_IncludeFifoIsSkippedNotRead(t *testing.T) {
	tasksRoot := t.TempDir()
	fifoPath := filepath.Join(tasksRoot, "a.fifo")
	if err := syscall.Mkfifo(fifoPath, 0644); err != nil {
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

// TestHash_InDirFifoIsSkipped covers walkTree's own entry classification
// (used for the main dir walk and directory includes) with a real stat
// (d.Info()) rather than the raw directory-entry type (d.Type()) — the
// latter can report 0 ("unknown", indistinguishable from "regular") on
// filesystems that don't populate d_type, which would otherwise let a FIFO
// slip through IsRegular() and into a blocking os.ReadFile. This doesn't
// reproduce the DT_UNKNOWN condition itself (filesystem-dependent, not
// under test control), but pins that a FIFO sitting directly in the walked
// dir is still skipped, not hashed as content, after the d.Info() switch.
func TestHash_InDirFifoIsSkipped(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "task-a")
	writeTaskFiles(t, dir, "name: a\n", "js")
	fifoPath := filepath.Join(dir, "a.fifo")
	if err := syscall.Mkfifo(fifoPath, 0644); err != nil {
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
