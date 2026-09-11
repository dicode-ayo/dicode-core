package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSpawnDetached_UnwritableLogFails: the caller decides what an unusable
// log means — ensureDaemon retries with the output discarded rather than
// leaving the user daemonless — so this must surface as an error, not start a
// daemon whose output silently goes nowhere.
func TestSpawnDetached_UnwritableLogFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if _, err := SpawnDetached("dicode.yaml", 0, filepath.Join(dir, "sub", "daemon.log")); err == nil {
		t.Fatal("SpawnDetached must fail when it cannot create the log")
	}
}

// TestAbsConfigPath_ResolvesRelative: the config path is handed to a process
// that may be started from a different directory, so it must not stay
// relative.
func TestAbsConfigPath_ResolvesRelative(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	got := absConfigPath("dicode.yaml")
	if want := filepath.Join(dir, "dicode.yaml"); got != want {
		t.Errorf("absConfigPath() = %q; want %q", got, want)
	}
}
