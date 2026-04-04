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

	ctx, cancel := context.WithCancel(context.Background())
	StartCleanup(ctx, nil, interval, maxAge, log)

	// Cancel immediately and verify the goroutine exits without hanging the test.
	cancel()

	// Give the goroutine a moment to exit.
	time.Sleep(50 * time.Millisecond)
	// If we reach here without deadlock the test passes.
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

	// Wait longer than the interval to ensure cleanup would have fired if enabled.
	time.Sleep(50 * time.Millisecond)

	if _, err := os.Stat(path); err != nil {
		t.Errorf("file was deleted even though DICODE_DEBUG is set: %s", path)
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
