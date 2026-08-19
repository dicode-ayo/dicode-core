package taskset

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/dicode/dicode/pkg/task"
)

// A taskset override's permissions.fs path must be ${VAR}-expanded the same way
// the base task.yaml's own paths are. Before this was wired, an override's path
// reached the sandbox as the literal string "${DATADIR}/dev-clones", which
// matches no real directory — so the grant silently denied every access instead
// of failing loudly (buildin/auto-fix and buildin/task-create both shipped with
// exactly one such dead grant).
func TestResolve_OverrideFSPathIsTemplateExpanded(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	taskDir := filepath.Join(root, "tasks", "agent")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}

	writeFile(t, taskDir, "task.yaml", `
apiVersion: dicode/v1
kind: Task
name: agent
runtime: deno
entrypoint: task.js
trigger:
  manual: true
permissions:
  fs:
    - path: "${DATADIR}/base-grant"
      permission: r
`)
	writeFile(t, taskDir, "task.js", "export default async function () {}\n")

	tsPath := writeFile(t, filepath.Join(root, "tasks"), "taskset.yaml", `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: ts
spec:
  entries:
    agent-override:
      ref:
        path: ./agent/task.yaml
      overrides:
        fs:
          - path: "${DATADIR}/dev-clones"
            permission: rw
`)

	r := NewResolver(dataDir, false, zap.NewNop())
	resolved, failures, err := r.Resolve(t.Context(), "", &Ref{Path: tsPath}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(failures) > 0 {
		t.Fatalf("unexpected failures: %+v", failures)
	}

	var got *task.Spec
	for _, rt := range resolved {
		if spec, ok := rt.Kinded.(*task.Spec); ok && strings.HasSuffix(spec.ID, "agent-override") {
			got = spec
		}
	}
	if got == nil {
		t.Fatalf("entry not resolved; got %d tasks", len(resolved))
	}
	if len(got.Permissions.FS) != 1 {
		t.Fatalf("fs grants = %d, want 1 (override replaces the base list)", len(got.Permissions.FS))
	}

	want := filepath.Join(dataDir, "dev-clones")
	if got.Permissions.FS[0].Path != want {
		t.Errorf("override fs path = %q, want %q", got.Permissions.FS[0].Path, want)
	}
	if strings.Contains(got.Permissions.FS[0].Path, "${") {
		t.Errorf("override fs path still holds an unexpanded template var: %q", got.Permissions.FS[0].Path)
	}
}

// Expansion must not write machine-specific absolute paths back into the parsed
// taskset — the raw-config editor and the approval diff render that config back
// to the operator, who wrote ${DATADIR}.
func TestResolve_OverrideExpansionLeavesParsedConfigUntouched(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	taskDir := filepath.Join(root, "tasks", "agent")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, taskDir, "task.yaml", `
apiVersion: dicode/v1
kind: Task
name: agent
runtime: deno
entrypoint: task.js
trigger:
  manual: true
`)
	writeFile(t, taskDir, "task.js", "export default async function () {}\n")

	tsPath := writeFile(t, filepath.Join(root, "tasks"), "taskset.yaml", `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: ts
spec:
  entries:
    agent-override:
      ref:
        path: ./agent/task.yaml
      overrides:
        fs:
          - path: "${DATADIR}/dev-clones"
            permission: rw
`)

	ts, err := LoadTaskSet(tsPath)
	if err != nil {
		t.Fatalf("LoadTaskSet: %v", err)
	}
	entry := ts.Spec.Entries["agent-override"]
	if entry == nil || entry.Overrides == nil || len(entry.Overrides.Fs) != 1 {
		t.Fatalf("fixture did not parse as expected")
	}

	r := NewResolver(dataDir, false, zap.NewNop())
	if _, _, err := r.Resolve(t.Context(), "", &Ref{Path: tsPath}, nil, nil, nil); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// Re-load and confirm the on-disk/parsed override still reads as authored.
	ts2, err := LoadTaskSet(tsPath)
	if err != nil {
		t.Fatalf("LoadTaskSet (2): %v", err)
	}
	if got := ts2.Spec.Entries["agent-override"].Overrides.Fs[0].Path; got != "${DATADIR}/dev-clones" {
		t.Errorf("parsed override path = %q, want the authored %q", got, "${DATADIR}/dev-clones")
	}
}
