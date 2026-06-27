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
func runGuardScript(t *testing.T, pol guardPolicy, payload string) (string, error) {
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

func TestGuard_FSSymlinkDenied(t *testing.T) {
	link := filepath.Join(t.TempDir(), "link")
	pol := guardPolicy{Net: guardNet{Mode: "unrestricted"}, Run: guardRun{Mode: "deny"}}
	out, err := runGuardScript(t, pol, fmt.Sprintf("import os\nos.symlink('/etc/passwd', %q)", link))
	requireDenied(t, out, err, "permissions.fs")
	if _, statErr := os.Lstat(link); statErr == nil {
		t.Error("denied symlink was still created")
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
	// A var not in env_allowed must not be readable.
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
    pass  # expected
# .get() must also return None
assert os.environ.get("SECRET_NOT_DECLARED") is None, "get must return None for filtered var"
# Membership test must also be filtered
assert "SECRET_NOT_DECLARED" not in os.environ, "in-test must return False for filtered var"
`
	out, err := runGuardScript(t, pol, payload)
	requireAllowed(t, out, err)
}

func TestGuard_EnvReadAllowed_DeclaredVar(t *testing.T) {
	// A declared var that is in os.environ must be readable.
	pol := guardPolicy{
		Net:        guardNet{Mode: "unrestricted"},
		Run:        guardRun{Mode: "deny"},
		EnvAllowed: []string{"PATH", "HOME", "MY_DECLARED_VAR", "DICODE_SOCKET", "DICODE_TOKEN"},
	}
	payload := `
import os
os.environ["MY_DECLARED_VAR"] = "hello"  # set it first
v = os.environ["MY_DECLARED_VAR"]
assert v == "hello", f"expected 'hello', got {v!r}"
`
	out, err := runGuardScript(t, pol, payload)
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
os.environ["ANYTHING"] = "value"
v = os.environ.get("ANYTHING")
assert v == "value", f"expected 'value', got {v!r}"
`
	out, err := runGuardScript(t, pol, payload)
	requireAllowed(t, out, err)
}

func TestGuard_EnvIter_OnlyDeclared(t *testing.T) {
	// Iterating os.environ must only yield allowed keys.
	pol := guardPolicy{
		Net:        guardNet{Mode: "unrestricted"},
		Run:        guardRun{Mode: "deny"},
		EnvAllowed: []string{"ONLY_THIS_ONE", "DICODE_SOCKET", "DICODE_TOKEN"},
	}
	payload := `
import os
os.environ["ONLY_THIS_ONE"] = "yes"
os.environ["NOT_THIS_ONE"] = "no"
keys = list(os.environ.keys())
assert "ONLY_THIS_ONE" in keys, f"ONLY_THIS_ONE missing from {keys}"
assert "NOT_THIS_ONE" not in keys, f"NOT_THIS_ONE leaked into {keys}"
`
	out, err := runGuardScript(t, pol, payload)
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
