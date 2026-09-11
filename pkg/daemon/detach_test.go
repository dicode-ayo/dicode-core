package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
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

// TestDetachHandoff_RequestCancels: a detach and a Ctrl-C look identical to
// everything downstream of the root context; the flag is what tells Run to
// leave a replacement behind rather than simply exit.
func TestDetachHandoff_RequestCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &detachHandoff{cancel: cancel}

	if d.requested.Load() {
		t.Fatal("a fresh handoff must not read as requested")
	}
	d.request()
	d.request() // idempotent — the keystroke and the CLI verb can race

	if !d.requested.Load() {
		t.Fatal("request must mark the handoff")
	}
	<-ctx.Done()
}

// TestNewDetachHandoff_UnarmedInBackground: Ctrl-\ only makes sense where
// someone can press it, and a daemon with no terminal must keep Go's SIGQUIT
// goroutine dump.
func TestNewDetachHandoff_UnarmedInBackground(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()

	if d := newDetachHandoff(cancel, zap.NewNop(), false); d.foreground {
		t.Fatal("handoff armed SIGQUIT for a daemon with no controlling terminal")
	}
}
