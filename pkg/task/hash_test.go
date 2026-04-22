package task

import (
	"os"
	"path/filepath"
	"testing"
)

// Tests for pkg/task.Hash and pkg/task.ScanDir.
// Covers issue #125 item 4: "identical content on reload produces the same
// hash → no spurious re-registration" — the reconciler diffs the Hash output
// for each task on every sync pass, so non-determinism here would cause
// every poll tick to re-register every task.

func writeTaskFiles(t *testing.T, dir, yaml, js string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatalf("write task.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.js"), []byte(js), 0644); err != nil {
		t.Fatalf("write task.js: %v", err)
	}
}

func TestHash_StableAcrossInvocations(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hello")
	writeTaskFiles(t, dir,
		"name: hello\ntrigger:\n  manual: true\n",
		"export default () => 'ok'\n",
	)

	const trials = 10
	first, err := Hash(dir)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	for i := 1; i < trials; i++ {
		got, err := Hash(dir)
		if err != nil {
			t.Fatalf("Hash trial %d: %v", i, err)
		}
		if got != first {
			t.Fatalf("Hash non-deterministic on trial %d: got %q, want %q", i, got, first)
		}
	}
}

func TestHash_ChangesOnYamlEdit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hello")
	writeTaskFiles(t, dir, "name: hello\n", "export default () => 'ok'\n")

	before, err := Hash(dir)
	if err != nil {
		t.Fatalf("before: %v", err)
	}

	// Rewrite task.yaml with different content.
	if err := os.WriteFile(filepath.Join(dir, "task.yaml"), []byte("name: hello-modified\n"), 0644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	after, err := Hash(dir)
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if before == after {
		t.Fatalf("Hash did not change after YAML edit: both = %q", before)
	}
}

func TestHash_ChangesOnScriptEdit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hello")
	writeTaskFiles(t, dir, "name: hello\n", "export default () => 1\n")

	before, err := Hash(dir)
	if err != nil {
		t.Fatalf("before: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "task.js"), []byte("export default () => 2\n"), 0644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	after, err := Hash(dir)
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if before == after {
		t.Fatalf("Hash did not change after script edit: both = %q", before)
	}
}

// TestHash_IncludesTsEdit is a regression gate for the bug tracked in #157:
// pkg/task/hash.go currently only reads task.yaml + task.js, so editing a
// Deno task's task.ts (the project default) produces the same hash and the
// reconciler never re-registers. This test is skipped until #157 is fixed;
// it's here so the fix can flip `t.Skip` → real assertion in one line.
func TestHash_IncludesTsEdit(t *testing.T) {
	t.Skip("#157: hash.go does not include task.ts — Deno script edits go undetected")

	dir := filepath.Join(t.TempDir(), "hello")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.yaml"), []byte("name: hello\n"), 0644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.ts"), []byte("v1"), 0644); err != nil {
		t.Fatalf("write ts v1: %v", err)
	}
	before, err := Hash(dir)
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.ts"), []byte("v2"), 0644); err != nil {
		t.Fatalf("write ts v2: %v", err)
	}
	after, err := Hash(dir)
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if before == after {
		t.Fatalf("Hash ignored task.ts edit (#157): before == after == %q", before)
	}
}

// TestHash_FilenameInjectionBarrier guards the "include filename as
// separator" comment in hash.go: hash(A_yaml + B_js) must not collide with
// hash(A_yaml + B_js_content_as_yaml), i.e. shuffling bytes across files
// must produce a different digest.
func TestHash_FilenameInjectionBarrier(t *testing.T) {
	a := filepath.Join(t.TempDir(), "a")
	b := filepath.Join(t.TempDir(), "b")

	// Same bytes, different filename assignment.
	writeTaskFiles(t, a, "ALPHA", "BETA")
	writeTaskFiles(t, b, "BETA", "ALPHA") // swapped

	ha, err := Hash(a)
	if err != nil {
		t.Fatalf("hash a: %v", err)
	}
	hb, err := Hash(b)
	if err != nil {
		t.Fatalf("hash b: %v", err)
	}
	if ha == hb {
		t.Fatalf("hashes collided despite filename swap: %q", ha)
	}
}

// TestScanDir_StableAcrossInvocations covers the reconciler's broader
// assumption: scanning the same tasks directory must produce the same
// taskID → hash map every time, otherwise the diff() output would be
// non-deterministic and the registry would churn on every poll tick.
func TestScanDir_StableAcrossInvocations(t *testing.T) {
	root := t.TempDir()
	writeTaskFiles(t, filepath.Join(root, "alpha"),
		"name: alpha\n", "export default () => 'a'\n")
	writeTaskFiles(t, filepath.Join(root, "beta"),
		"name: beta\n", "export default () => 'b'\n")

	first, err := ScanDir(root)
	if err != nil {
		t.Fatalf("ScanDir: %v", err)
	}
	for i := 0; i < 5; i++ {
		got, err := ScanDir(root)
		if err != nil {
			t.Fatalf("ScanDir trial %d: %v", i, err)
		}
		if len(got) != len(first) {
			t.Fatalf("trial %d: len=%d, want %d", i, len(got), len(first))
		}
		for id, hash := range first {
			if got[id] != hash {
				t.Fatalf("trial %d: id=%q hash=%q, want %q", i, id, got[id], hash)
			}
		}
	}
}

// TestScanDir_SkipsDirsWithoutTaskYaml documents that ScanDir silently
// ignores directories missing task.yaml — paired with the reconciler's
// assumption that "scratch" dirs in a repo don't register as tasks.
func TestScanDir_SkipsDirsWithoutTaskYaml(t *testing.T) {
	root := t.TempDir()
	writeTaskFiles(t, filepath.Join(root, "real"),
		"name: real\n", "export default () => 1\n")

	// Sibling dir with JS but no task.yaml.
	scratch := filepath.Join(root, "scratch")
	if err := os.MkdirAll(scratch, 0755); err != nil {
		t.Fatalf("mkdir scratch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scratch, "task.js"), []byte("nope"), 0644); err != nil {
		t.Fatalf("write scratch js: %v", err)
	}

	got, err := ScanDir(root)
	if err != nil {
		t.Fatalf("ScanDir: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 task, got %d: %v", len(got), got)
	}
	if _, ok := got["real"]; !ok {
		t.Errorf("missing 'real' task in result: %v", got)
	}
	if _, ok := got["scratch"]; ok {
		t.Errorf("scratch dir without task.yaml should not register")
	}
}
