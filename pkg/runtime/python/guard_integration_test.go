package python

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// pythonForGuardTest locates an interpreter for guard integration tests. The
// guard only needs the stdlib, so a plain python3 suffices; uv is accepted as
// a fallback for environments that only ship the managed toolchain. Skips
// locally when neither is present; on CI it fails loudly (same contract as
// TestPythonSDK).
func pythonForGuardTest(t *testing.T) []string {
	t.Helper()
	if py, err := exec.LookPath("python3"); err == nil {
		return []string{py}
	}
	if uv, err := exec.LookPath("uv"); err == nil {
		return []string{uv, "run", "--no-project"}
	}
	if os.Getenv("CI") != "" {
		t.Fatal("neither python3 nor uv on PATH in CI; setup step missing?")
	}
	t.Skip("neither python3 nor uv on PATH; skipping guard integration tests")
	return nil
}

// runGuardScript renders the guard for pol, appends payload, and executes the
// result with a real interpreter. Returns the combined output and the exec
// error (nil on exit 0).
func runGuardScript(t *testing.T, pol guardPolicy, payload string, extraEnv ...string) (string, error) {
	t.Helper()
	guard, err := buildGuard(pol)
	if err != nil {
		t.Fatalf("buildGuard: %v", err)
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "guarded.py")
	if err := os.WriteFile(script, []byte(guard+"\n"+payload+"\n"), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	interp := pythonForGuardTest(t)
	cmd := exec.Command(interp[0], append(interp[1:], script)...) //nolint:gosec
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	out, runErr := cmd.CombinedOutput()
	return string(out), runErr
}

func requireDenied(t *testing.T, out string, err error, permField string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected PermissionError exit, got success; output:\n%s", out)
	}
	if !strings.Contains(out, "PermissionError") || !strings.Contains(out, permField) {
		t.Fatalf("expected PermissionError naming %s; output:\n%s", permField, out)
	}
}

func requireAllowed(t *testing.T, out string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("expected success, got %v; output:\n%s", err, out)
	}
}

func TestGuard_FSWriteDenied(t *testing.T) {
	target := filepath.Join(t.TempDir(), "denied.txt")
	pol := guardPolicy{
		Net: guardNet{Mode: "unrestricted"},
		Run: guardRun{Mode: "deny"},
	}
	out, err := runGuardScript(t, pol, fmt.Sprintf("open(%q, 'w')", target))
	requireDenied(t, out, err, "permissions.fs")
	if _, statErr := os.Stat(target); statErr == nil {
		t.Error("denied write still created the file")
	}
}

func TestGuard_FSWriteAllowedInDeclaredDir(t *testing.T) {
	dir := t.TempDir()
	pol := guardPolicy{
		Net:     guardNet{Mode: "unrestricted"},
		Run:     guardRun{Mode: "deny"},
		FSWrite: []string{dir},
	}
	payload := fmt.Sprintf(`
import os
p = os.path.join(%q, "ok.txt")
with open(p, "w") as f:
    f.write("hi")
os.remove(p)
os.mkdir(os.path.join(%q, "sub"))
`, dir, dir)
	out, err := runGuardScript(t, pol, payload)
	requireAllowed(t, out, err)
}

func TestGuard_FSRemoveDenied(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.txt")
	if err := os.WriteFile(victim, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	pol := guardPolicy{Net: guardNet{Mode: "unrestricted"}, Run: guardRun{Mode: "deny"}}
	out, err := runGuardScript(t, pol, fmt.Sprintf("import os\nos.remove(%q)", victim))
	requireDenied(t, out, err, "permissions.fs")
}

// TestGuard_FSDenyWinsOverCoveringWriteRoot proves the deny set beats a broad
// write grant covering its directory: with fs_write on the config dir and
// fs_deny on the lock inside it, a direct os.remove of the lock is rejected.
// This is the issue #402 escalation on the Python runtime — a broad-fs task
// dropping dicode.lock to force a bootstrap re-seed.
func TestGuard_FSDenyWinsOverCoveringWriteRoot(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "dicode.lock")
	if err := os.WriteFile(lock, []byte("approved: original"), 0o600); err != nil {
		t.Fatal(err)
	}
	pol := guardPolicy{
		Net:     guardNet{Mode: "unrestricted"},
		Run:     guardRun{Mode: "deny"},
		FSWrite: []string{dir},
		FSDeny:  []string{lock},
	}
	out, err := runGuardScript(t, pol, fmt.Sprintf("import os\nos.remove(%q)", lock))
	requireDenied(t, out, err, "permissions.fs")
	if _, statErr := os.Stat(lock); statErr != nil {
		t.Errorf("lock was removed despite fs_deny: %v", statErr)
	}
}

// TestGuard_FSDenyDirectoryCoversNestedFile proves the deny set covers a
// denied *directory*, not just an exact-path match: with fs_write on the
// data dir and fs_deny on a subdirectory within it (mirroring the
// approval-snapshot cache dir added in protectedPaths), a write to a new
// file nested one level inside that denied directory is rejected even
// though the file's own realpath never equals the denied directory's
// realpath.
func TestGuard_FSDenyDirectoryCoversNestedFile(t *testing.T) {
	dir := t.TempDir()
	deniedDir := filepath.Join(dir, "approval-snapshots")
	if err := os.Mkdir(deniedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(deniedDir, "abc123.json")
	pol := guardPolicy{
		Net:     guardNet{Mode: "unrestricted"},
		Run:     guardRun{Mode: "deny"},
		FSWrite: []string{dir},
		FSDeny:  []string{deniedDir},
	}
	out, err := runGuardScript(t, pol, fmt.Sprintf("open(%q, 'w')", nested))
	requireDenied(t, out, err, "permissions.fs")
	if _, statErr := os.Stat(nested); statErr == nil {
		t.Error("write nested inside denied directory still created the file")
	}
}

// TestGuard_FSHardlinkSourceDenied proves os.link checks the *source* path
// (args[0]) against fs_deny, not just the destination. Unlike os.symlink, a
// hardlink makes the new name an alias for the SAME inode as the source —
// there is no realpath indirection to "unmask" the source at write time — so
// without a source-side check a task with any writable directory could
// hardlink a denied file into it and then write through the alias to mutate
// the denied file's real content, defeating fs_deny entirely.
func TestGuard_FSHardlinkSourceDenied(t *testing.T) {
	writeDir := t.TempDir()
	denyDir := t.TempDir()
	victim := filepath.Join(denyDir, "protected.json")
	if err := os.WriteFile(victim, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(writeDir, "alias.json")
	pol := guardPolicy{
		Net:     guardNet{Mode: "unrestricted"},
		Run:     guardRun{Mode: "deny"},
		FSWrite: []string{writeDir},
		FSDeny:  []string{victim},
	}
	out, err := runGuardScript(t, pol, fmt.Sprintf("import os\nos.link(%q, %q)", victim, alias))
	requireDenied(t, out, err, "permissions.fs")
	if _, statErr := os.Lstat(alias); statErr == nil {
		t.Error("hardlink of denied source was still created")
	}
	content, readErr := os.ReadFile(victim)
	if readErr != nil {
		t.Fatalf("read victim: %v", readErr)
	}
	if string(content) != "original" {
		t.Errorf("victim content mutated: got %q", content)
	}
}

func TestGuard_FSSymlinkDenied(t *testing.T) {
	link := filepath.Join(t.TempDir(), "link")
	pol := guardPolicy{Net: guardNet{Mode: "unrestricted"}, Run: guardRun{Mode: "deny"}}
	out, err := runGuardScript(t, pol, fmt.Sprintf("import os\nos.symlink('/etc/passwd', %q)", link))
	requireDenied(t, out, err, "permissions.fs")
	if _, statErr := os.Lstat(link); statErr == nil {
		t.Error("denied symlink was still created")
	}
}

// TestGuard_FSOpenDirFdBypassDenied proves os.open cannot be used to escape
// fs_write/fs_deny via dir_fd: the "open" audit event's args are documented
// as exactly (path, mode, flags) — dir_fd is never included — so the guard
// only ever sees the bare relative filename. The script os.chdir()s into its
// one writable directory first, so on unfixed guard.py the path-only check
// resolves that bare relative name against a cwd inside the write grant and
// (wrongly) allows it, while the real syscall actually resolves the name
// against denied_fd and creates the file inside the denied directory
// instead — a full bypass despite the check appearing to pass.
func TestGuard_FSOpenDirFdBypassDenied(t *testing.T) {
	writeDir := t.TempDir()
	deniedDir := t.TempDir()
	forged := filepath.Join(deniedDir, "forged.json")
	pol := guardPolicy{
		Net:     guardNet{Mode: "unrestricted"},
		Run:     guardRun{Mode: "deny"},
		FSWrite: []string{writeDir},
	}
	payload := fmt.Sprintf(`
import os
os.chdir(%q)
denied_fd = os.open(%q, os.O_RDONLY | os.O_DIRECTORY)
fd = os.open("forged.json", os.O_WRONLY | os.O_CREAT, dir_fd=denied_fd)
os.close(fd)
`, writeDir, deniedDir)
	out, err := runGuardScript(t, pol, payload)
	requireDenied(t, out, err, "dir_fd")
	if _, statErr := os.Stat(forged); statErr == nil {
		t.Error("dir_fd-relative os.open still created the file inside the denied directory")
	}
	if _, statErr := os.Stat(filepath.Join(writeDir, "forged.json")); statErr == nil {
		t.Error("unexpected: file landed in the writable dir rather than the denied dir — test setup is wrong")
	}
}

// TestGuard_FSLinkDirFdBypassDenied proves os.link's dst_dir_fd argument
// (args[3] of the "os.link" audit event) is checked, not just the bare
// destination path string. As with the os.open test above, the script
// chdir()s into its writable directory first so the unfixed path-only check
// on the bare relative destination name would (wrongly) pass, while the
// real hardlink lands inside the denied directory via dst_dir_fd.
func TestGuard_FSLinkDirFdBypassDenied(t *testing.T) {
	writeDir := t.TempDir()
	deniedDir := t.TempDir()
	src := filepath.Join(writeDir, "src.txt")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	forged := filepath.Join(deniedDir, "forged-link.txt")
	pol := guardPolicy{
		Net:     guardNet{Mode: "unrestricted"},
		Run:     guardRun{Mode: "deny"},
		FSWrite: []string{writeDir},
	}
	payload := fmt.Sprintf(`
import os
os.chdir(%q)
denied_fd = os.open(%q, os.O_RDONLY | os.O_DIRECTORY)
os.link(%q, "forged-link.txt", dst_dir_fd=denied_fd)
`, writeDir, deniedDir, src)
	out, err := runGuardScript(t, pol, payload)
	requireDenied(t, out, err, "dir_fd")
	if _, statErr := os.Lstat(forged); statErr == nil {
		t.Error("dir_fd-relative os.link still created the hardlink inside the denied directory")
	}
}

// TestGuard_FSMkdirDirFdBypassDenied proves os.mkdir's dir_fd argument
// (args[2] of the "os.mkdir" audit event, which is (path, mode, dir_fd)) is
// checked, not just the bare path string. As above, chdir into the writable
// dir first so the unfixed path-only check on the bare relative name would
// (wrongly) pass while the real mkdir(2) lands inside the denied directory.
func TestGuard_FSMkdirDirFdBypassDenied(t *testing.T) {
	writeDir := t.TempDir()
	deniedDir := t.TempDir()
	forged := filepath.Join(deniedDir, "forged-subdir")
	pol := guardPolicy{
		Net:     guardNet{Mode: "unrestricted"},
		Run:     guardRun{Mode: "deny"},
		FSWrite: []string{writeDir},
	}
	payload := fmt.Sprintf(`
import os
os.chdir(%q)
denied_fd = os.open(%q, os.O_RDONLY | os.O_DIRECTORY)
os.mkdir("forged-subdir", dir_fd=denied_fd)
`, writeDir, deniedDir)
	out, err := runGuardScript(t, pol, payload)
	requireDenied(t, out, err, "dir_fd")
	if _, statErr := os.Stat(forged); statErr == nil {
		t.Error("dir_fd-relative os.mkdir still created the directory inside the denied directory")
	}
}

func TestGuard_FSReadAlwaysAllowed(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "readable.txt")
	if err := os.WriteFile(src, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	// No fs grants at all: reads must still work (reads are unenforced).
	pol := guardPolicy{Net: guardNet{Mode: "unrestricted"}, Run: guardRun{Mode: "deny"}}
	payload := fmt.Sprintf("assert open(%q).read() == 'data'", src)
	out, err := runGuardScript(t, pol, payload)
	requireAllowed(t, out, err)
}

func TestGuard_SubprocessDenied(t *testing.T) {
	pol := guardPolicy{Net: guardNet{Mode: "unrestricted"}, Run: guardRun{Mode: "deny"}}
	payload := "import subprocess, sys\nsubprocess.run([sys.executable, '-c', 'pass'])"
	out, err := runGuardScript(t, pol, payload)
	requireDenied(t, out, err, "permissions.run")
}

func TestGuard_SubprocessAllowedByWildcard(t *testing.T) {
	pol := guardPolicy{Net: guardNet{Mode: "unrestricted"}, Run: guardRun{Mode: "allow"}}
	payload := "import subprocess, sys\nsubprocess.run([sys.executable, '-c', 'pass'], check=True)"
	out, err := runGuardScript(t, pol, payload)
	requireAllowed(t, out, err)
}

func TestGuard_SubprocessAllowlistMatchesBasename(t *testing.T) {
	pol := guardPolicy{
		Net: guardNet{Mode: "unrestricted"},
		Run: guardRun{Mode: "allowlist", Commands: []string{"true"}},
	}
	tru, err := exec.LookPath("true")
	if err != nil {
		t.Skip("true not on PATH")
	}
	payload := fmt.Sprintf("import subprocess\nsubprocess.run([%q], check=True)", tru)
	out, runErr := runGuardScript(t, pol, payload)
	requireAllowed(t, out, runErr)

	payload = "import subprocess, sys\nsubprocess.run([sys.executable, '-c', 'pass'])"
	out, runErr = runGuardScript(t, pol, payload)
	requireDenied(t, out, runErr, "permissions.run")
}

func TestGuard_OsSystemDenied(t *testing.T) {
	pol := guardPolicy{Net: guardNet{Mode: "unrestricted"}, Run: guardRun{Mode: "deny"}}
	out, err := runGuardScript(t, pol, "import os\nos.system('true')")
	requireDenied(t, out, err, "permissions.run")
}

// localConnectPayload binds a listener on 127.0.0.1 (bind is ungoverned) and
// connects to it, exercising the socket.connect audit event without any
// external network dependency.
const localConnectPayload = `
import socket
srv = socket.socket()
srv.bind(("127.0.0.1", 0))
srv.listen(1)
c = socket.socket()
c.connect(srv.getsockname())
c.close()
srv.close()
`

func TestGuard_NetDenyBlocksConnect(t *testing.T) {
	pol := guardPolicy{Net: guardNet{Mode: "deny"}, Run: guardRun{Mode: "deny"}}
	out, err := runGuardScript(t, pol, localConnectPayload)
	requireDenied(t, out, err, "permissions.net")
}

func TestGuard_NetUnrestrictedAllowsConnect(t *testing.T) {
	pol := guardPolicy{Net: guardNet{Mode: "unrestricted"}, Run: guardRun{Mode: "deny"}}
	out, err := runGuardScript(t, pol, localConnectPayload)
	requireAllowed(t, out, err)
}

func TestGuard_NetAllowlistPassesIPLiterals(t *testing.T) {
	// Hostname vetting happens at getaddrinfo; raw IP connects pass the
	// allowlist by design (documented guardrail limitation).
	pol := guardPolicy{
		Net: guardNet{Mode: "allowlist", Hosts: []string{"api.example.com"}},
		Run: guardRun{Mode: "deny"},
	}
	out, err := runGuardScript(t, pol, localConnectPayload)
	requireAllowed(t, out, err)
}

func TestGuard_EnvReadDenied(t *testing.T) {
	// A pre-seeded var not in env_allowed must not be readable.
	pol := guardPolicy{
		Net:        guardNet{Mode: "unrestricted"},
		Run:        guardRun{Mode: "deny"},
		EnvAllowed: []string{"PATH", "HOME", "DICODE_SOCKET", "DICODE_TOKEN"},
	}
	payload := `
import os
try:
    v = os.environ["SECRET_NOT_DECLARED"]
    raise AssertionError(f"should have raised KeyError, got {v!r}")
except KeyError:
    pass  # expected: pre-seeded but undeclared
# .get() must also return None
assert os.environ.get("SECRET_NOT_DECLARED") is None, "get must return None for filtered var"
# Membership test must also be filtered
assert "SECRET_NOT_DECLARED" not in os.environ, "in-test must return False for filtered var"
`
	// Seed SECRET_NOT_DECLARED into the subprocess env so the test proves the
	// filter hides a var that actually exists in the process environment.
	out, err := runGuardScript(t, pol, payload, "SECRET_NOT_DECLARED=shh")
	requireAllowed(t, out, err)
}

func TestGuard_EnvReadAllowed_DeclaredVar(t *testing.T) {
	// A declared var pre-seeded by the test harness must be readable.
	pol := guardPolicy{
		Net:        guardNet{Mode: "unrestricted"},
		Run:        guardRun{Mode: "deny"},
		EnvAllowed: []string{"PATH", "HOME", "MY_DECLARED_VAR", "DICODE_SOCKET", "DICODE_TOKEN"},
	}
	payload := `
import os
v = os.environ["MY_DECLARED_VAR"]
assert v == "hello", f"expected 'hello', got {v!r}"
`
	// Seed MY_DECLARED_VAR into the subprocess environment so the test exercises
	// filtering against a var that genuinely exists in the inherited env.
	out, err := runGuardScript(t, pol, payload, "MY_DECLARED_VAR=hello")
	requireAllowed(t, out, err)
}

func TestGuard_EnvReadAll_NoFilter(t *testing.T) {
	// When env_allowed is nil (env_read_exposed=true), os.environ is unfiltered.
	pol := guardPolicy{
		Net:        guardNet{Mode: "unrestricted"},
		Run:        guardRun{Mode: "deny"},
		EnvAllowed: nil, // no filter
	}
	payload := `
import os
v = os.environ.get("ANYTHING")
assert v == "value", f"expected 'value', got {v!r}"
`
	// Seed ANYTHING so the test exercises reading a pre-existing env var in
	// unfiltered mode (rather than writing inside the payload and reading back).
	out, err := runGuardScript(t, pol, payload, "ANYTHING=value")
	requireAllowed(t, out, err)
}

func TestGuard_EnvIter_OnlyDeclaredAndWritten(t *testing.T) {
	// Iterating os.environ yields only declared keys plus keys the task wrote.
	// A pre-existing var not in env_allowed is hidden; a task-written var is
	// visible immediately after the write (write-then-read consistency).
	pol := guardPolicy{
		Net:        guardNet{Mode: "unrestricted"},
		Run:        guardRun{Mode: "deny"},
		EnvAllowed: []string{"DECLARED_VAR", "DICODE_SOCKET", "DICODE_TOKEN"},
	}
	payload := `
import os
os.environ["TASK_WRITTEN_VAR"] = "also_yes"  # not pre-declared but task wrote it
keys = list(os.environ.keys())
assert "DECLARED_VAR" in keys, f"DECLARED_VAR missing from {keys}"
assert "TASK_WRITTEN_VAR" in keys, f"TASK_WRITTEN_VAR missing — task should read back what it wrote"
assert "HIDDEN_PREEXISTING_VAR" not in keys, f"HIDDEN_PREEXISTING_VAR leaked in {keys}"
# Read back what was written
assert os.environ["TASK_WRITTEN_VAR"] == "also_yes", "write-then-read must work"
# Declared var was seeded by the test harness and must be readable
assert os.environ["DECLARED_VAR"] == "yes", "pre-seeded declared var must be readable"
`
	// Seed DECLARED_VAR and a hidden undeclared var to prove the filter works
	// against vars that genuinely exist in the inherited process environment.
	out, err := runGuardScript(t, pol, payload, "DECLARED_VAR=yes", "HIDDEN_PREEXISTING_VAR=secret")
	requireAllowed(t, out, err)
}

func TestGuard_EnvDelete_UndeclaredDenied(t *testing.T) {
	// Deleting a pre-seeded var not in env_allowed must raise KeyError.
	pol := guardPolicy{
		Net:        guardNet{Mode: "unrestricted"},
		Run:        guardRun{Mode: "deny"},
		EnvAllowed: []string{"DICODE_SOCKET", "DICODE_TOKEN"},
	}
	payload := `
import os
try:
    del os.environ["UNDECLARED_SECRET"]
    raise AssertionError("should have raised KeyError")
except KeyError:
    pass  # expected: cannot delete what you cannot see
`
	// Seed UNDECLARED_SECRET so the test proves the filter blocks deletion of a
	// var that genuinely exists in the inherited process environment.
	out, err := runGuardScript(t, pol, payload, "UNDECLARED_SECRET=secret")
	requireAllowed(t, out, err)
}

func TestGuard_NetAllowlistGetaddrinfo(t *testing.T) {
	pol := guardPolicy{
		Net: guardNet{Mode: "allowlist", Hosts: []string{"localhost"}},
		Run: guardRun{Mode: "deny"},
	}
	// localhost resolves locally (no external DNS); the denied hostname
	// must raise before any resolution is attempted.
	payload := `
import socket
socket.getaddrinfo("localhost", 80)
try:
    socket.getaddrinfo("denied.example.com", 80)
except PermissionError:
    pass
else:
    raise SystemExit("expected PermissionError for undeclared host")
`
	out, err := runGuardScript(t, pol, payload)
	requireAllowed(t, out, err)
}
