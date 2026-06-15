package deno

import (
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
	args := buildDenoArgs(specWithEnv([]task.EnvEntry{{Name: "FOO"}, {Name: "BAR"}}), "/run/sock", "/shim.ts", "/runner.ts")
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
	args := buildDenoArgs(specWithEnv(nil), "/run/sock", "/shim.ts", "/runner.ts")
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
	args := buildDenoArgs(spec, "/run/sock", "/shim.ts", "/runner.ts")
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
	args := buildDenoArgs(spec, "/run/sock", "/shim.ts", "/runner.ts")
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
	args := buildDenoArgs(specWithEnv([]task.EnvEntry{{Name: "DICODE_DATADIR"}}), "/run/sock", "/shim.ts", "/runner.ts")
	got, ok := allowEnvArg(args)
	if !ok {
		t.Fatal("no --allow-env arg emitted")
	}
	if got == "--allow-env" {
		t.Errorf("named entries without env_read_exposed must not grant bare --allow-env, got %q", got)
	}
}
