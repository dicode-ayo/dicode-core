package deno

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/task"
)

// findArg returns the value of the first --flag=value entry, or "" if absent.
func findArg(args []string, flag string) (string, bool) {
	prefix := flag + "="
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return strings.TrimPrefix(a, prefix), true
		}
	}
	return "", false
}

// TestBuildDenoArgs_ProtectedPathsDenied: even a broad --allow-write covering
// the config dir must not expose dicode.lock/dicode.yaml — they appear in
// --deny-write, which Deno applies with precedence over any allow.
func TestBuildDenoArgs_ProtectedPathsDenied(t *testing.T) {
	spec := &task.Spec{
		ID:      "t/broad",
		TaskDir: "/srv/tasks/broad",
		Permissions: task.Permissions{
			FS: []task.FSEntry{{Path: "/etc/dicode", Permission: "w"}},
		},
	}
	protected := []string{"/etc/dicode/dicode.lock", "/etc/dicode/dicode.yaml"}

	args := buildDenoArgs(spec, "/tmp/d.sock", "/tmp/shim.ts", "/tmp/runner.ts", protected)

	deny, ok := findArg(args, "--deny-write")
	if !ok {
		t.Fatalf("expected --deny-write flag; args = %v", args)
	}
	for _, p := range protected {
		if !strings.Contains(deny, p) {
			t.Errorf("--deny-write missing protected path %q; got %q", p, deny)
		}
	}
	// The broad grant is still honored — legitimate broad-fs tasks keep working.
	allow, ok := findArg(args, "--allow-write")
	if !ok || !strings.Contains(allow, "/etc/dicode") {
		t.Errorf("expected broad --allow-write to remain; got %q", allow)
	}
}

// TestBuildDenoArgs_NoProtectedPaths: with no protected paths wired (legacy /
// test callers), no --deny-write flag is emitted.
func TestBuildDenoArgs_NoProtectedPaths(t *testing.T) {
	spec := &task.Spec{ID: "t/x", TaskDir: "/srv/tasks/x"}
	args := buildDenoArgs(spec, "/tmp/d.sock", "/tmp/shim.ts", "/tmp/runner.ts", nil)
	if v, ok := findArg(args, "--deny-write"); ok {
		t.Errorf("expected no --deny-write with nil protected paths; got %q", v)
	}
}

// TestBuildDenoArgs_FSPathCleaned: a "../" segment in a declared fs path is
// normalized by filepath.Clean before reaching --allow-write.
func TestBuildDenoArgs_FSPathCleaned(t *testing.T) {
	spec := &task.Spec{
		ID:      "t/dirty",
		TaskDir: "/srv/tasks/dirty",
		Permissions: task.Permissions{
			FS: []task.FSEntry{{Path: "/data/foo/../bar", Permission: "w"}},
		},
	}
	args := buildDenoArgs(spec, "/tmp/d.sock", "/tmp/shim.ts", "/tmp/runner.ts", nil)
	allow, _ := findArg(args, "--allow-write")
	if !strings.Contains(allow, "/data/bar") {
		t.Errorf("expected cleaned /data/bar in --allow-write; got %q", allow)
	}
	if strings.Contains(allow, "..") {
		t.Errorf("--allow-write still contains unresolved '..'; got %q", allow)
	}
}

// TestBuildDenoArgs_RelativeFSPathCleaned: a relative path with "../" is joined
// to TaskDir and then cleaned.
func TestBuildDenoArgs_RelativeFSPathCleaned(t *testing.T) {
	spec := &task.Spec{
		ID:      "t/rel",
		TaskDir: "/srv/tasks/rel",
		Permissions: task.Permissions{
			FS: []task.FSEntry{{Path: "sub/../out", Permission: "rw"}},
		},
	}
	args := buildDenoArgs(spec, "/tmp/d.sock", "/tmp/shim.ts", "/tmp/runner.ts", nil)
	allow, _ := findArg(args, "--allow-write")
	if !strings.Contains(allow, "/srv/tasks/rel/out") {
		t.Errorf("expected /srv/tasks/rel/out in --allow-write; got %q", allow)
	}
}

// TestEnforcement_ProtectedLockNotWritable proves end-to-end on the pinned
// Deno that --deny-write overrides a broad --allow-write: a task granted write
// over the directory holding dicode.lock still cannot overwrite the lock. This
// is the issue #402 escalation — a broad-fs task self-approving via the lock.
func TestEnforcement_ProtectedLockNotWritable(t *testing.T) {
	e := newTestEnv(t)

	configDir := t.TempDir()
	lockPath := filepath.Join(configDir, "dicode.lock")
	if err := os.WriteFile(lockPath, []byte("approved: original"), 0o600); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	e.rt.SetProtectedPaths([]string{lockPath})

	spec := &task.Spec{
		ID: "lock-attack", Name: "lock-attack", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true}, Timeout: 30 * time.Second,
		Permissions: task.Permissions{
			// Broad write over the whole config dir — would cover the lock
			// without the deny-write belt.
			FS: []task.FSEntry{{Path: configDir, Permission: "w"}},
		},
	}
	r := e.runSpec(t, `
		export default async function main() {
			try {
				await Deno.writeTextFile(`+"`"+lockPath+"`"+`, "approved: ATTACKER");
				return "allowed";
			} catch (e) {
				return (e && e.name) || "error";
			}
		}
	`, spec)
	if r.Error != nil {
		t.Fatalf("unexpected run error: %v", r.Error)
	}
	got, _ := r.ReturnValue.(string)
	if !strings.Contains(got, "NotCapable") {
		t.Errorf("expected NotCapable denial writing the lock, got %v", r.ReturnValue)
	}
	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read lock back: %v", err)
	}
	if string(data) != "approved: original" {
		t.Errorf("lock was modified despite deny-write: %q", string(data))
	}
}

// TestEnforcement_ProtectedLockNotRemovable proves --deny-write also blocks a
// direct Deno.remove of the lock: deletion is a write to the protected path, so
// a broad-fs task cannot drop the lock to force a bootstrap re-seed on restart.
func TestEnforcement_ProtectedLockNotRemovable(t *testing.T) {
	e := newTestEnv(t)

	configDir := t.TempDir()
	lockPath := filepath.Join(configDir, "dicode.lock")
	if err := os.WriteFile(lockPath, []byte("approved: original"), 0o600); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	e.rt.SetProtectedPaths([]string{lockPath})

	spec := &task.Spec{
		ID: "lock-remove", Name: "lock-remove", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true}, Timeout: 30 * time.Second,
		Permissions: task.Permissions{
			FS: []task.FSEntry{{Path: configDir, Permission: "w"}},
		},
	}
	r := e.runSpec(t, `
		export default async function main() {
			try {
				await Deno.remove(`+"`"+lockPath+"`"+`);
				return "allowed";
			} catch (e) {
				return (e && e.name) || "error";
			}
		}
	`, spec)
	if r.Error != nil {
		t.Fatalf("unexpected run error: %v", r.Error)
	}
	got, _ := r.ReturnValue.(string)
	if !strings.Contains(got, "NotCapable") {
		t.Errorf("expected NotCapable denial removing the lock, got %v", r.ReturnValue)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Errorf("lock was removed despite deny-write: %v", err)
	}
}
