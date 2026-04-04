package deno

// Design notes:
//
// Each call to Run creates a temp file under os.TempDir() with the prefix
// "dicode-shim-" and removes it via defer os.Remove. If the daemon crashes
// mid-run those defers never execute and files accumulate in /tmp.
//
// StartCleanup runs a background goroutine that periodically scans
// os.TempDir() for files matching "dicode-shim-*" and deletes any whose
// modification time is older than maxAge. Using mtime as the criterion is
// conservative: a file that is still being written or read will have a
// recent mtime and will not be touched.
//
// The activePIDs callback is reserved for future use (e.g. skip files owned
// by a still-running Deno process). Passing nil is valid and safe.
//
// Cleanup is best-effort: errors are logged but never cause a crash. If the
// DICODE_DEBUG environment variable is set the goroutine exits immediately so
// that temp files remain available for inspection.

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"
)

const tempFileGlob = "dicode-shim-*"

// StartCleanup starts a background goroutine that periodically removes
// orphaned dicode deno temp files from the OS temp directory.
//
// Parameters:
//   - ctx: cancel to stop the goroutine.
//   - activePIDs: optional callback that returns PIDs of currently-running
//     Deno processes; reserved for future use, may be nil.
//   - interval: how often to scan (e.g. 10 * time.Minute).
//   - maxAge: files older than this are eligible for deletion (e.g. 30 * time.Minute).
//
// If the DICODE_DEBUG environment variable is non-empty, cleanup is skipped
// so that temp files remain available for manual inspection.
func StartCleanup(ctx context.Context, activePIDs func() []int, interval time.Duration, maxAge time.Duration, log *zap.Logger) {
	if os.Getenv("DICODE_DEBUG") != "" {
		log.Debug("deno temp cleanup disabled (DICODE_DEBUG is set)")
		return
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cleanupOnce(maxAge, log)
			}
		}
	}()
}

// cleanupOnce performs a single scan of os.TempDir() and removes eligible files.
func cleanupOnce(maxAge time.Duration, log *zap.Logger) {
	pattern := filepath.Join(os.TempDir(), tempFileGlob)
	matches, err := filepath.Glob(pattern)
	if err != nil {
		log.Debug("deno temp cleanup: glob error", zap.Error(err))
		return
	}

	now := time.Now()
	for _, path := range matches {
		info, err := os.Stat(path)
		if err != nil {
			// File may have been removed between Glob and Stat — not an error.
			log.Debug("deno temp cleanup: stat skipped", zap.String("path", path), zap.Error(err))
			continue
		}
		if now.Sub(info.ModTime()) <= maxAge {
			// Too recent — skip regardless of prefix (safety margin).
			continue
		}
		if err := os.Remove(path); err != nil {
			log.Debug("deno temp cleanup: remove failed", zap.String("path", path), zap.Error(err))
			continue
		}
		log.Debug("deno temp cleanup: removed orphaned temp file", zap.String("path", path))
	}
}
