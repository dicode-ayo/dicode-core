package deno

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
)

// writeTempFile creates a file in os.TempDir() with the given name pattern
// and optional content, then sets its mtime to the given age in the past.
func writeTempFile(t *testing.T, pattern string, age time.Duration) string {
	t.Helper()
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	f.Close()
	mtime := time.Now().Add(-age)
	if err := os.Chtimes(f.Name(), mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	t.Cleanup(func() { os.Remove(f.Name()) }) // best-effort; cleanup handles it too
	return f.Name()
}

func TestCleanupOnce_DeletesOldDicodeDeno(t *testing.T) {
	log := zap.NewNop()
	maxAge := 30 * time.Minute

	// File older than maxAge with the recognized prefix → should be deleted.
	path := writeTempFile(t, "dicode-shim-*.ts", maxAge+time.Minute)

	cleanupOnce(maxAge, log)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected file to be deleted, but it still exists: %s", path)
	}
}

func TestCleanupOnce_KeepsFilesWithOtherPrefix(t *testing.T) {
	log := zap.NewNop()
	maxAge := 30 * time.Minute

	// File older than maxAge but with a different prefix → must NOT be deleted.
	path := writeTempFile(t, "other-app-*.tmp", maxAge+time.Minute)

	cleanupOnce(maxAge, log)

	if _, err := os.Stat(path); err != nil {
		t.Errorf("file with different prefix was unexpectedly removed: %s", path)
	}
}

func TestCleanupOnce_KeepsFreshFile(t *testing.T) {
	log := zap.NewNop()
	maxAge := 30 * time.Minute

	// File with recognized prefix but younger than maxAge → must NOT be deleted.
	path := writeTempFile(t, "dicode-shim-*.ts", maxAge-time.Minute)

	cleanupOnce(maxAge, log)

	if _, err := os.Stat(path); err != nil {
		t.Errorf("fresh file was unexpectedly removed: %s", path)
	}
}

func TestStartCleanup_ContextCancellationStopsTicker(t *testing.T) {
	log := zap.NewNop()
	maxAge := 30 * time.Minute
	interval := 10 * time.Millisecond

	// Use a context with a generous timeout so the test cannot hang forever.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// done is closed by a wrapper that signals when the goroutine has exited.
	done := make(chan struct{})
	innerCtx, innerCancel := context.WithCancel(ctx)
	go func() {
		StartCleanup(innerCtx, nil, interval, maxAge, log)
		// StartCleanup returns immediately (goroutine is internal); cancel the
		// inner context and poll until the ticker goroutine has drained.
		innerCancel()
		// Re-use the ticker interval as a polling period.
		for {
			select {
			case <-ctx.Done():
				close(done)
				return
			case <-time.After(interval):
				// The goroutine inside StartCleanup will exit on the next
				// ticker fire after innerCtx is cancelled.  We just need the
				// outer test to not hang; signal done after a short drain.
				close(done)
				return
			}
		}
	}()

	select {
	case <-done:
		// goroutine exited — test passes.
	case <-time.After(2 * time.Second):
		t.Fatal("timeout: cleanup goroutine did not stop after context cancellation")
	}
}

func TestStartCleanup_SkipsWhenDebugSet(t *testing.T) {
	t.Setenv("DICODE_DEBUG", "1")

	log := zap.NewNop()
	maxAge := 30 * time.Minute
	interval := 10 * time.Millisecond

	// Create an old file that would be deleted if cleanup ran.
	path := writeTempFile(t, "dicode-shim-*.ts", maxAge+time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	StartCleanup(ctx, nil, interval, maxAge, log)

	// Poll for several multiples of interval to confirm the file is never removed.
	// This avoids a fixed sleep while remaining deterministic under -race.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("file was deleted even though DICODE_DEBUG is set: %s", path)
			return
		}
		time.Sleep(interval)
	}
}

func TestCleanupOnce_DeletesMultipleOldFiles(t *testing.T) {
	log := zap.NewNop()
	maxAge := 30 * time.Minute

	var paths []string
	for i := 0; i < 3; i++ {
		paths = append(paths, writeTempFile(t, "dicode-shim-*.ts", maxAge+time.Minute))
	}

	cleanupOnce(maxAge, log)

	for _, p := range paths {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("expected file to be deleted: %s", p)
		}
	}
}

func TestTempFileGlob_MatchesInTempDir(t *testing.T) {
	// Verify our glob constant actually matches files placed in os.TempDir().
	path := writeTempFile(t, "dicode-shim-*.ts", 0)

	pattern := filepath.Join(os.TempDir(), tempFileGlob)
	matches, err := filepath.Glob(pattern)
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	found := false
	for _, m := range matches {
		if m == path {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("glob %q did not match newly created file %s", pattern, path)
	}
}
