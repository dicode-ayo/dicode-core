package task

import (
	"os"
	"path/filepath"
	"testing"
)

// Tests for pkg/task.ComputeContentHash and ComputeSpecHash — the shared
// dir+resolved combination primitive pkg/approval and pkg/taskset both build
// their own content hash on top of (see #688).

const testContentHashDomain = "dicode-contenthash-test-v1"

func TestComputeContentHash_SameDirAndResolved_SameHash(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "task-a")
	writeTaskFiles(t, dir, "name: a\n", "js")
	resolved := map[string]string{"runtime": "deno"}

	h1, err := ComputeContentHash(testContentHashDomain, "a", dir, resolved)
	if err != nil {
		t.Fatalf("ComputeContentHash: %v", err)
	}
	h2, err := ComputeContentHash(testContentHashDomain, "a", dir, resolved)
	if err != nil {
		t.Fatalf("ComputeContentHash: %v", err)
	}
	if h1 != h2 {
		t.Errorf("hash not stable across identical calls: %q != %q", h1, h2)
	}
}

func TestComputeContentHash_ChangedFileContent_DifferentHash(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "task-a")
	writeTaskFiles(t, dir, "name: a\n", "// v1")
	resolved := map[string]string{"runtime": "deno"}

	before, err := ComputeContentHash(testContentHashDomain, "a", dir, resolved)
	if err != nil {
		t.Fatalf("ComputeContentHash: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "task.js"), []byte("// v2"), 0644); err != nil {
		t.Fatal(err)
	}

	after, err := ComputeContentHash(testContentHashDomain, "a", dir, resolved)
	if err != nil {
		t.Fatalf("ComputeContentHash: %v", err)
	}
	if before == after {
		t.Errorf("hash unchanged after editing a file in dir: %q", before)
	}
}

func TestComputeContentHash_ChangedResolvedValue_DifferentHash(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "task-a")
	writeTaskFiles(t, dir, "name: a\n", "js")

	before, err := ComputeContentHash(testContentHashDomain, "a", dir, map[string]string{"runtime": "deno"})
	if err != nil {
		t.Fatalf("ComputeContentHash: %v", err)
	}
	after, err := ComputeContentHash(testContentHashDomain, "a", dir, map[string]string{"runtime": "python"})
	if err != nil {
		t.Fatalf("ComputeContentHash: %v", err)
	}
	if before == after {
		t.Errorf("hash unchanged after changing resolved value: %q", before)
	}
}

// TestComputeContentHash_IncludeEscapingSiblingScope_Errors mirrors the
// symlink-escape fixture in TestHash_IncludeThroughSymlinkedIntermediateDirIsRejected
// (pkg/task/hash_test.go) and TestSource_ScriptEditThroughSymlinkEscapedHashIncludeEmitsUpdate
// (pkg/taskset/source_test.go): a hash_include entry that escapes its
// sibling-task boundary only once a symlink is resolved must make
// ComputeContentHash return a non-nil error — this used to be untestable at
// the taskset change-detection level because the failure was silently
// swallowed there (#682); it's directly testable at the shared primitive now.
func TestComputeContentHash_IncludeEscapingSiblingScope_Errors(t *testing.T) {
	parent := t.TempDir()
	tasksRoot := filepath.Join(parent, "tasks-root")
	if err := os.MkdirAll(tasksRoot, 0755); err != nil {
		t.Fatalf("mkdir tasksRoot: %v", err)
	}
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(outside, 0755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("host-secret"), 0644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	// A symlinked directory INSIDE tasksRoot (the sibling-task boundary)
	// that physically redirects outside it. Lexically,
	// "../evil-link/secret.txt" looks contained within tasksRoot — only
	// resolving the symlink reveals it actually escapes to outside/secret.txt.
	evilLink := filepath.Join(tasksRoot, "evil-link")
	if err := os.Symlink(outside, evilLink); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	dir := filepath.Join(tasksRoot, "task-a")
	writeTaskFiles(t, dir, "name: a\n", "js")

	_, err := ComputeContentHash(testContentHashDomain, "a", dir, nil, "../evil-link/secret.txt")
	if err == nil {
		t.Fatal("ComputeContentHash: want error for hash_include escaping the sibling-task boundary through a symlink, got nil")
	}
}

func TestComputeSpecHash_DiffersWhenValueDiffers_StableWhenUnchanged(t *testing.T) {
	a1, err := ComputeSpecHash("a", map[string]string{"foo": "bar"})
	if err != nil {
		t.Fatalf("ComputeSpecHash: %v", err)
	}
	a2, err := ComputeSpecHash("a", map[string]string{"foo": "bar"})
	if err != nil {
		t.Fatalf("ComputeSpecHash: %v", err)
	}
	if a1 != a2 {
		t.Errorf("hash not stable across identical calls: %q != %q", a1, a2)
	}

	b, err := ComputeSpecHash("a", map[string]string{"foo": "baz"})
	if err != nil {
		t.Fatalf("ComputeSpecHash: %v", err)
	}
	if a1 == b {
		t.Errorf("hash unchanged after changing v: %q", a1)
	}
}
