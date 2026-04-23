package deno

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/task"
)

// Tests in this file address dicode-core#123: per-runtime permissions sandbox
// enforcement. The issue lists a 4×5 matrix (Deno/Python/Docker/Podman × env,
// fs-read, fs-write, net, apis). Existing coverage in runtime_test.go already
// pins the Deno env and net halves (TestRuntime_Env_* and TestRuntime_Net_*);
// this file fills the remaining fs and apis gaps for Deno. Python, Docker,
// and Podman are out of scope here — see the top of the file for each one's
// t.Skip-worthy reason and the follow-up tracker.
//
// Python has no in-tree test harness (see the comment at the top of
// pkg/runtime/python/runtime.go re: uv + Python availability); Docker and
// Podman runtimes enforce via `--network` and bind-mount shapes which are
// container-platform-level rather than language-runtime-level — testing
// them meaningfully requires a real Docker/Podman socket and isn't covered
// here. A follow-up issue will track those three runtimes if they stay in
// alpha scope.

// ── fs: read ──────────────────────────────────────────────────────────────

// TestEnforcement_Fs_ReadAllowed: a declared read path works — the happy path.
// Pairs with _ReadDenied below to assert the allow-list is actually consulted.
func TestEnforcement_Fs_ReadAllowed(t *testing.T) {
	e := newTestEnv(t)

	// Create a file in a dedicated directory and declare that directory as
	// readable. t.TempDir cleans itself up.
	readDir := t.TempDir()
	filePath := filepath.Join(readDir, "hello.txt")
	if err := os.WriteFile(filePath, []byte("declared-readable"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	spec := &task.Spec{
		ID: "fs-read-allow", Name: "fs-read-allow", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true}, Timeout: 30 * time.Second,
		Permissions: task.Permissions{
			FS: []task.FSEntry{{Path: readDir, Permission: "r"}},
		},
	}
	r := e.runSpec(t, `
		export default async function main() {
			return await Deno.readTextFile(`+"`"+filePath+"`"+`);
		}
	`, spec)
	if r.Error != nil {
		t.Fatalf("run error: %v", r.Error)
	}
	if r.ReturnValue != "declared-readable" {
		t.Errorf("expected file contents, got %v", r.ReturnValue)
	}
}

// TestEnforcement_Fs_ReadDenied: reading outside the declared read paths
// must fail at the Deno sandbox layer (NotCapable). Catches a regression
// where runtime.go stops translating spec.Permissions.FS into --allow-read.
func TestEnforcement_Fs_ReadDenied(t *testing.T) {
	e := newTestEnv(t)

	// Create a file in a directory NOT declared in Permissions.FS.
	undeclaredDir := t.TempDir()
	secret := filepath.Join(undeclaredDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("should-not-be-readable"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	spec := &task.Spec{
		ID: "fs-read-deny", Name: "fs-read-deny", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true}, Timeout: 30 * time.Second,
		Permissions: task.Permissions{
			// No FS entries at all — nothing declared outside the default
			// task-dir read scope runtime.go grants for script loading.
		},
	}
	r := e.runSpec(t, `
		export default async function main() {
			try {
				await Deno.readTextFile(`+"`"+secret+"`"+`);
				return "allowed";
			} catch (e) {
				return (e && e.name) || "error";
			}
		}
	`, spec)
	if r.Error != nil {
		t.Fatalf("unexpected run error: %v", r.Error)
	}
	if r.ReturnValue == "allowed" || r.ReturnValue == "should-not-be-readable" {
		t.Errorf("undeclared read path was allowed — sandbox bypass: got %v", r.ReturnValue)
	}
	// Deno's permission denial surfaces as a NotCapable error. Anything
	// else (compile error, missing file at a surprising layer, etc.) is
	// a different failure mode that would silently satisfy the "not
	// allowed" assertion above and mask a real regression where the
	// sandbox stopped denying. Require the specific error class.
	got, ok := r.ReturnValue.(string)
	if !ok {
		t.Errorf("unexpected return value type: %T %v", r.ReturnValue, r.ReturnValue)
	} else if !strings.Contains(got, "NotCapable") {
		t.Errorf("expected NotCapable-flavoured sandbox denial, got %q", got)
	}
}

// ── fs: write ─────────────────────────────────────────────────────────────

func TestEnforcement_Fs_WriteAllowed(t *testing.T) {
	e := newTestEnv(t)

	writeDir := t.TempDir()
	outPath := filepath.Join(writeDir, "output.txt")

	spec := &task.Spec{
		ID: "fs-write-allow", Name: "fs-write-allow", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true}, Timeout: 30 * time.Second,
		Permissions: task.Permissions{
			FS: []task.FSEntry{{Path: writeDir, Permission: "w"}},
		},
	}
	r := e.runSpec(t, `
		export default async function main() {
			await Deno.writeTextFile(`+"`"+outPath+"`"+`, "written-ok");
			return "done";
		}
	`, spec)
	if r.Error != nil {
		t.Fatalf("run error: %v", r.Error)
	}
	if r.ReturnValue != "done" {
		t.Errorf("expected 'done', got %v", r.ReturnValue)
	}

	// Sanity: the file actually landed on disk with the expected bytes.
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read back written file: %v", err)
	}
	if string(got) != "written-ok" {
		t.Errorf("file contents = %q, want %q", got, "written-ok")
	}
}

func TestEnforcement_Fs_WriteDenied(t *testing.T) {
	e := newTestEnv(t)

	undeclaredDir := t.TempDir()
	outPath := filepath.Join(undeclaredDir, "should-not-exist.txt")

	spec := &task.Spec{
		ID: "fs-write-deny", Name: "fs-write-deny", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true}, Timeout: 30 * time.Second,
		Permissions: task.Permissions{
			// No writable FS entries declared.
		},
	}
	r := e.runSpec(t, `
		export default async function main() {
			try {
				await Deno.writeTextFile(`+"`"+outPath+"`"+`, "should-never-write");
				return "allowed";
			} catch (e) {
				return (e && e.name) || "error";
			}
		}
	`, spec)
	if r.Error != nil {
		t.Fatalf("unexpected run error: %v", r.Error)
	}
	if r.ReturnValue == "allowed" {
		t.Errorf("undeclared write path was allowed — sandbox bypass")
	}
	// The file must not have been created.
	if _, err := os.Stat(outPath); err == nil {
		t.Errorf("write sandbox bypassed: file %q exists", outPath)
	} else if !os.IsNotExist(err) {
		t.Errorf("stat outPath: %v", err)
	}
}

// ── apis: dicode.run_task gating ──────────────────────────────────────────

// TestEnforcement_Apis_RunTaskDenied: a task without Permissions.Dicode.Tasks
// (nil) must be refused when calling dicode.run_task. The IPC server's
// capability check short-circuits before the engine is ever called — which
// means this test needs no engine mock, the reject-at-boundary behaviour
// is what we're actually pinning.
func TestEnforcement_Apis_RunTaskDenied(t *testing.T) {
	e := newTestEnv(t)

	spec := &task.Spec{
		ID: "api-runtask-deny", Name: "api-runtask-deny", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true}, Timeout: 30 * time.Second,
		Permissions: task.Permissions{
			// No Dicode block at all — dicode.run_task must be denied.
			// (Nil Dicode means all dicode.* / mcp.* APIs are off.)
		},
	}
	// dicode.run_task returns a Promise; we catch the rejection and surface
	// a stable marker string so the assertion below doesn't depend on the
	// exact error format from either the SDK or the IPC server.
	r := e.runSpec(t, `
		export default async function main({ dicode }) {
			try {
				await dicode.run_task("any-task-id", {});
				return "allowed";
			} catch (e) {
				return "rejected:" + (e && e.message ? e.message : String(e));
			}
		}
	`, spec)
	if r.Error != nil {
		t.Fatalf("unexpected run error: %v", r.Error)
	}
	got, ok := r.ReturnValue.(string)
	if !ok {
		t.Fatalf("unexpected return value type: %T %v", r.ReturnValue, r.ReturnValue)
	}
	if got == "allowed" {
		t.Error("dicode.run_task was allowed without Permissions.Dicode.Tasks — capability check bypassed")
	}
	if !strings.HasPrefix(got, "rejected:") {
		t.Errorf("expected 'rejected:<reason>', got %q", got)
	}
	// The IPC server replies with the specific string
	// "ipc: permission denied (tasks.trigger)" when CapTaskTrigger is
	// absent (see pkg/ipc/server.go:505). Substring-matching that text
	// keeps this assertion from accepting "rejected: some other error"
	// if the task code itself threw before reaching the IPC boundary.
	if !strings.Contains(got, "tasks.trigger") {
		t.Errorf("expected error to cite the 'tasks.trigger' capability gate, got %q", got)
	}
}
