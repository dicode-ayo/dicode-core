package deno

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/task"
)

// allowEnvArg extracts the --allow-env flag (with or without a value) from a
// buildDenoArgs result. Returns ("", false) if no --allow-env arg is present.
func allowEnvArg(args []string) (string, bool) {
	for _, a := range args {
		if a == "--allow-env" || strings.HasPrefix(a, "--allow-env=") {
			return a, true
		}
	}
	return "", false
}

func specWithEnv(env []task.EnvEntry) *task.Spec {
	return &task.Spec{
		ID: "env-args", Name: "env-args", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true}, Timeout: 30 * time.Second,
		TaskDir:     "/tmp/task",
		Permissions: task.Permissions{Env: env},
	}
}

// TestBuildDenoArgs_Env_List: declared names produce an explicit allowlist that
// always carries the internal IPC + cache vars plus the declared names.
func TestBuildDenoArgs_Env_List(t *testing.T) {
	args := buildDenoArgs(specWithEnv([]task.EnvEntry{{Name: "FOO"}, {Name: "BAR"}}), "/run/sock", "/shim.ts", "/runner.ts", nil)
	got, ok := allowEnvArg(args)
	if !ok {
		t.Fatal("no --allow-env arg emitted")
	}
	if !strings.HasPrefix(got, "--allow-env=") {
		t.Fatalf("expected an explicit list, got bare flag %q", got)
	}
	for _, want := range []string{"DICODE_SOCKET", "DICODE_TOKEN", "HOME", "DENO_DIR", "XDG_CACHE_HOME", "FOO", "BAR"} {
		if !strings.Contains(got, want) {
			t.Errorf("allow-env %q missing %q", got, want)
		}
	}
}

// TestBuildDenoArgs_Env_Omitted: no declared env still yields the baseline
// allowlist (never bare --allow-env).
func TestBuildDenoArgs_Env_Omitted(t *testing.T) {
	args := buildDenoArgs(specWithEnv(nil), "/run/sock", "/shim.ts", "/runner.ts", nil)
	got, ok := allowEnvArg(args)
	if !ok {
		t.Fatal("no --allow-env arg emitted")
	}
	if got == "--allow-env" {
		t.Fatalf("omitted env must not grant bare --allow-env, got %q", got)
	}
	if !strings.Contains(got, "DICODE_SOCKET") {
		t.Errorf("baseline allowlist missing DICODE_SOCKET: %q", got)
	}
}

// TestBuildDenoArgs_Env_ReadExposed: env_read_exposed grants bare --allow-env.
func TestBuildDenoArgs_Env_ReadExposed(t *testing.T) {
	spec := specWithEnv(nil)
	spec.Permissions.EnvReadExposed = true
	args := buildDenoArgs(spec, "/run/sock", "/shim.ts", "/runner.ts", nil)
	got, ok := allowEnvArg(args)
	if !ok {
		t.Fatal("no --allow-env arg emitted")
	}
	if got != "--allow-env" {
		t.Errorf("env_read_exposed must grant bare --allow-env, got %q", got)
	}
}

// TestBuildDenoArgs_Env_ReadExposedWithNamed: env_read_exposed grants bare
// --allow-env regardless of named entries — read permission is widened while
// the named entries drive value forwarding (SubprocessEnv), tested separately.
func TestBuildDenoArgs_Env_ReadExposedWithNamed(t *testing.T) {
	spec := specWithEnv([]task.EnvEntry{{Name: "DICODE_DATADIR"}})
	spec.Permissions.EnvReadExposed = true
	args := buildDenoArgs(spec, "/run/sock", "/shim.ts", "/runner.ts", nil)
	got, ok := allowEnvArg(args)
	if !ok {
		t.Fatal("no --allow-env arg emitted")
	}
	if got != "--allow-env" {
		t.Errorf("env_read_exposed with named entries must grant bare --allow-env, got %q", got)
	}
}

// TestBuildDenoArgs_Env_NamedOnlyNeverBare: named entries without
// env_read_exposed must produce an explicit allowlist, never bare --allow-env.
func TestBuildDenoArgs_Env_NamedOnlyNeverBare(t *testing.T) {
	args := buildDenoArgs(specWithEnv([]task.EnvEntry{{Name: "DICODE_DATADIR"}}), "/run/sock", "/shim.ts", "/runner.ts", nil)
	got, ok := allowEnvArg(args)
	if !ok {
		t.Fatal("no --allow-env arg emitted")
	}
	if got == "--allow-env" {
		t.Errorf("named entries without env_read_exposed must not grant bare --allow-env, got %q", got)
	}
}

// hasArg returns true if args contains the exact string s.
func hasArg(args []string, s string) bool {
	for _, a := range args {
		if a == s {
			return true
		}
	}
	return false
}

// hasArgPrefix returns true if any arg starts with prefix.
func hasArgPrefix(args []string, prefix string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

// TestFindDenoLockFile_TaskDir: lockfile in the task dir itself is found at depth 0.
func TestFindDenoLockFile_TaskDir(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "deno.lock")
	if err := os.WriteFile(lock, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	got := findDenoLockFile(dir, 3)
	if got != lock {
		t.Errorf("expected %q, got %q", lock, got)
	}
}

// TestFindDenoLockFile_ParentDir: lockfile 2 levels up (tasks/deno.lock pattern).
func TestFindDenoLockFile_ParentDir(t *testing.T) {
	root := t.TempDir()
	taskDir := filepath.Join(root, "buildin", "my-task")
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(root, "deno.lock")
	if err := os.WriteFile(lock, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	got := findDenoLockFile(taskDir, 3)
	if got != lock {
		t.Errorf("expected %q, got %q", lock, got)
	}
}

// TestFindDenoLockFile_Missing: no lockfile → empty string.
func TestFindDenoLockFile_Missing(t *testing.T) {
	dir := t.TempDir()
	got := findDenoLockFile(dir, 2)
	if got != "" {
		t.Errorf("expected empty string when no lockfile, got %q", got)
	}
}

// TestFindDenoLockFile_BeyondMaxParents: lockfile exactly at maxParents+1 is not found,
// but at maxParents it is — guards the loop boundary in both directions.
func TestFindDenoLockFile_BeyondMaxParents(t *testing.T) {
	root := t.TempDir()
	// 3 levels deep: root/a/b/c/ — root is 3 parents above taskDir.
	taskDir := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(root, "deno.lock")
	if err := os.WriteFile(lock, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	// maxParents=2: root is 3 up → not found (negative boundary).
	if got := findDenoLockFile(taskDir, 2); got != "" {
		t.Errorf("negative boundary: expected empty, got %q", got)
	}
	// maxParents=3: root is 3 up → found (positive boundary).
	if got := findDenoLockFile(taskDir, 3); got != lock {
		t.Errorf("positive boundary: expected %q, got %q", lock, got)
	}
}

// TestBuildDenoArgs_LockFrozen_WithLockfile: when a deno.lock exists at the
// buildin layout (2 levels up), --lock=<path> and --frozen must appear in the args.
func TestBuildDenoArgs_LockFrozen_WithLockfile(t *testing.T) {
	root := t.TempDir()
	taskDir := filepath.Join(root, "buildin", "relay-client")
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(root, "deno.lock")
	if err := os.WriteFile(lock, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}

	spec := &task.Spec{
		ID: "test", Name: "test", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true}, Timeout: 30 * time.Second,
		TaskDir: taskDir,
	}
	args := buildDenoArgs(spec, "/run/sock", "/shim.ts", "/runner.ts", nil)

	if !hasArgPrefix(args, "--lock=") {
		t.Error("expected --lock=<path> arg when deno.lock exists")
	}
	if !hasArg(args, "--frozen") {
		t.Error("expected --frozen arg when deno.lock exists")
	}
	for _, a := range args {
		if strings.HasPrefix(a, "--lock=") {
			if a != "--lock="+lock {
				t.Errorf("--lock path: got %q, want %q", a, "--lock="+lock)
			}
		}
	}
}

// TestBuildDenoArgs_LockFrozen_NoLockfile: no --lock or --frozen when no deno.lock found.
func TestBuildDenoArgs_LockFrozen_NoLockfile(t *testing.T) {
	spec := &task.Spec{
		ID: "test", Name: "test", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true}, Timeout: 30 * time.Second,
		TaskDir: t.TempDir(),
	}
	args := buildDenoArgs(spec, "/run/sock", "/shim.ts", "/runner.ts", nil)

	if hasArgPrefix(args, "--lock=") || hasArg(args, "--lock") {
		t.Error("--lock must not appear when no deno.lock is present")
	}
	if hasArg(args, "--frozen") {
		t.Error("--frozen must not appear when no deno.lock is present")
	}
}

// TestBuildDenoArgs_LockFrozen_SkippedWhenDenoJsonPresent: a deno.json in the task
// dir suppresses --lock/--frozen injection; Deno handles locking via deno.json.
func TestBuildDenoArgs_LockFrozen_SkippedWhenDenoJsonPresent(t *testing.T) {
	root := t.TempDir()
	taskDir := filepath.Join(root, "buildin", "my-task")
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		t.Fatal(err)
	}
	// deno.lock at parent — would be picked up without the deno.json check.
	if err := os.WriteFile(filepath.Join(root, "deno.lock"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	// deno.json in task dir signals the task manages its own lock policy.
	if err := os.WriteFile(filepath.Join(taskDir, "deno.json"), []byte(`{"lock":false}`), 0600); err != nil {
		t.Fatal(err)
	}

	spec := &task.Spec{
		ID: "test", Name: "test", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true}, Timeout: 30 * time.Second,
		TaskDir: taskDir,
	}
	args := buildDenoArgs(spec, "/run/sock", "/shim.ts", "/runner.ts", nil)

	if hasArgPrefix(args, "--lock=") || hasArg(args, "--lock") {
		t.Error("--lock must not appear when task has deno.json")
	}
	if hasArg(args, "--frozen") {
		t.Error("--frozen must not appear when task has deno.json")
	}
}
