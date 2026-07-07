package python

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	pkgruntime "github.com/dicode/dicode/pkg/runtime"
)

func TestLockSidecarPath(t *testing.T) {
	if got := LockSidecarPath("/x/tasks/t/task.py"); got != "/x/tasks/t/task.py.lock" {
		t.Fatalf("LockSidecarPath = %q", got)
	}
}

// The exact stderr uv writes when a `--locked` run is rejected because the
// script's inline metadata drifted from its lock sidecar. Pins that
// staleLockSignature keeps matching the real message.
const uvLockedStaleErr = "error: The lockfile at `uv.lock` needs to be updated, but `--locked` was provided. To update the lockfile, run `uv lock`.\n"

func TestStaleLockSignature_MatchesUvLockedError(t *testing.T) {
	s := pkgruntime.NewLockErrSniffer(staleLockSignature)
	s.Write([]byte(uvLockedStaleErr)) //nolint:errcheck
	if !s.StaleLock() {
		t.Errorf("staleLockSignature %q did not match uv's --locked error", staleLockSignature)
	}
}

func TestStaleLockSignature_IgnoresOrdinaryError(t *testing.T) {
	s := pkgruntime.NewLockErrSniffer(staleLockSignature)
	s.Write([]byte("Traceback (most recent call last):\n  File task.py, line 1\nValueError: boom\n")) //nolint:errcheck
	if s.StaleLock() {
		t.Error("ordinary Python traceback must not be treated as a stale lock")
	}
}

func TestHasInlineMetadata(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{"with block", "# /// script\n# dependencies = [\"httpx\"]\n# ///\nimport httpx\n", true},
		{"no block", "print('hello')\n", false},
		{"unterminated block", "# /// script\n# dependencies = []\nprint('x')\n", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasInlineMetadata([]byte(tc.src)); got != tc.want {
				t.Fatalf("HasInlineMetadata = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHasRequiresPython(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{"with requires-python", "# /// script\n# requires-python = \">=3.11\"\n# dependencies = [\"httpx\"]\n# ///\n", true},
		{"deps only, no requires-python", "# /// script\n# dependencies = [\"httpx\"]\n# ///\n", false},
		{"no block", "import httpx\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HasRequiresPython([]byte(tc.src)); got != tc.want {
				t.Fatalf("HasRequiresPython = %v, want %v", got, tc.want)
			}
		})
	}
}

// The runtime must pass --locked only when a lock sidecar exists for the
// task, and run plain otherwise — a Python task with no lock (e.g. no
// external deps) keeps working exactly as before (issue #465).
func TestBuildUvRunArgs(t *testing.T) {
	got := buildUvRunArgs("/tmp/w.py", true)
	if strings.Join(got, " ") != "run --locked /tmp/w.py" {
		t.Fatalf("locked args = %v", got)
	}
	got = buildUvRunArgs("/tmp/w.py", false)
	if strings.Join(got, " ") != "run /tmp/w.py" {
		t.Fatalf("unlocked args = %v", got)
	}
}

// stageLockSidecar copies the committed task.py.lock next to the temporary
// wrapper (uv discovers a script's lock strictly by <script>.lock filename),
// and reports false — not an error — when the task has no sidecar.
func TestStageLockSidecar(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "task.py")
	wrapper := filepath.Join(dir, "wrapper.py")

	// No sidecar → no staging, no error, run proceeds unlocked.
	staged, locked, err := stageLockSidecar(script, wrapper)
	if err != nil || locked || staged != "" {
		t.Fatalf("no sidecar: got (%q, %v, %v), want (\"\", false, nil)", staged, locked, err)
	}

	// Sidecar present → staged copy next to the wrapper.
	const lockBody = "version = 1\n"
	if err := os.WriteFile(script+".lock", []byte(lockBody), 0o644); err != nil {
		t.Fatal(err)
	}
	staged, locked, err = stageLockSidecar(script, wrapper)
	if err != nil || !locked {
		t.Fatalf("with sidecar: got (%q, %v, %v)", staged, locked, err)
	}
	if want := wrapper + ".lock"; staged != want {
		t.Fatalf("staged path = %q, want %q", staged, want)
	}
	data, err := os.ReadFile(staged)
	if err != nil {
		t.Fatalf("read staged: %v", err)
	}
	if string(data) != lockBody {
		t.Fatalf("staged content = %q, want %q", data, lockBody)
	}
}
