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
// the base task.yaml's own paths are. An unexpanded path reaches the sandbox as
// a literal "${DATADIR}/…" string, which matches no directory and denies every
// access silently rather than failing loudly.
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

// Expansion must not write machine-specific absolute paths back into the
// caller's override layers. Source holds its parentOverrides for the daemon's
// lifetime and re-resolves on every reconcile tick, so an in-place expansion
// would bake the first tick's paths into long-lived config state.
func TestResolve_DoesNotMutateCallerParentOverrides(t *testing.T) {
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
    agent:
      ref:
        path: ./agent/task.yaml
`)

	const authored = "${DATADIR}/dev-clones"
	parent := &Overrides{
		Entries: map[string]*Overrides{
			"agent": {Fs: []task.FSEntry{{Path: authored, Permission: "rw"}}},
		},
	}

	r := NewResolver(dataDir, false, zap.NewNop())
	// Twice: a second tick must see the same authored text the first one did.
	for tick := 1; tick <= 2; tick++ {
		resolved, failures, err := r.Resolve(t.Context(), "", &Ref{Path: tsPath}, nil, parent, nil)
		if err != nil {
			t.Fatalf("tick %d Resolve: %v", tick, err)
		}
		if len(failures) > 0 {
			t.Fatalf("tick %d unexpected failures: %+v", tick, failures)
		}

		var got *task.Spec
		for _, rt := range resolved {
			if spec, ok := rt.Kinded.(*task.Spec); ok {
				got = spec
			}
		}
		if got == nil || len(got.Permissions.FS) != 1 {
			t.Fatalf("tick %d: entry did not resolve with the override grant", tick)
		}
		if want := filepath.Join(dataDir, "dev-clones"); got.Permissions.FS[0].Path != want {
			t.Errorf("tick %d resolved path = %q, want %q", tick, got.Permissions.FS[0].Path, want)
		}
		if p := parent.Entries["agent"].Fs[0].Path; p != authored {
			t.Fatalf("tick %d mutated the caller's parentOverrides: %q, want %q", tick, p, authored)
		}
	}
}
