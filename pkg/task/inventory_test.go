package task

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func inventoryByPath(t *testing.T, files []FileMeta) map[string]FileMeta {
	t.Helper()
	out := make(map[string]FileMeta, len(files))
	for _, f := range files {
		out[f.Path] = f
	}
	return out
}

func TestInventoryListsFilesWithSizeAndHash(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "task.yaml"), "name: demo\n")
	writeFile(t, filepath.Join(dir, "task.js"), "export default () => 1;\n")
	writeFile(t, filepath.Join(dir, "lib", "helper.js"), "export const x = 1;\n")

	files, err := Inventory(dir)
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("want 3 entries, got %d: %+v", len(files), files)
	}

	// Sorted by path so the rendering is stable across polls.
	want := []string{"lib/helper.js", "task.js", "task.yaml"}
	for i, w := range want {
		if files[i].Path != w {
			t.Errorf("entry %d: want path %q, got %q", i, w, files[i].Path)
		}
	}

	byPath := inventoryByPath(t, files)
	yaml := byPath["task.yaml"]
	if yaml.Size != int64(len("name: demo\n")) {
		t.Errorf("task.yaml size: want %d, got %d", len("name: demo\n"), yaml.Size)
	}
	if yaml.Kind != FileKindRegular {
		t.Errorf("task.yaml kind: want %q, got %q", FileKindRegular, yaml.Kind)
	}
	if len(yaml.Hash) != 64 {
		t.Errorf("task.yaml hash: want a 64-char sha256, got %q", yaml.Hash)
	}
	if byPath["task.js"].Hash == yaml.Hash {
		t.Error("different contents must not share a hash")
	}
}

// The inventory's per-file hash is what makes a content change visible without
// rendering any bytes, so editing a file must move exactly that file's hash.
func TestInventoryHashTracksContent(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "task.yaml"), "name: demo\n")
	writeFile(t, filepath.Join(dir, "task.js"), "const a = 1;\n")

	before := inventoryByPath(t, mustInventory(t, dir))
	writeFile(t, filepath.Join(dir, "task.js"), "const a = 2;\n")
	after := inventoryByPath(t, mustInventory(t, dir))

	if before["task.js"].Hash == after["task.js"].Hash {
		t.Error("edited file kept its hash")
	}
	if before["task.yaml"].Hash != after["task.yaml"].Hash {
		t.Error("untouched file changed its hash")
	}
}

func TestInventorySkipsHeavyDirs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "task.yaml"), "name: demo\n")
	writeFile(t, filepath.Join(dir, "node_modules", "dep", "index.js"), "module.exports = 1;\n")
	writeFile(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/main\n")

	files := mustInventory(t, dir)
	if len(files) != 1 || files[0].Path != "task.yaml" {
		t.Fatalf("want only task.yaml, got %+v", files)
	}
}

// A symlink is never read through — its target string is the reviewable fact,
// matching how Hash folds it in.
func TestInventoryRecordsSymlinkTargetWithoutReading(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "task.yaml"), "name: demo\n")
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	byPath := inventoryByPath(t, mustInventory(t, dir))
	link, ok := byPath["link"]
	if !ok {
		t.Fatal("symlink missing from inventory")
	}
	if link.Kind != FileKindSymlink {
		t.Errorf("kind: want %q, got %q", FileKindSymlink, link.Kind)
	}
	if link.Target != "/etc/passwd" {
		t.Errorf("target: want /etc/passwd, got %q", link.Target)
	}
	if link.Hash != "" {
		t.Errorf("symlink must carry no content hash, got %q", link.Hash)
	}
}

// hash_include targets feed the content hash from outside the task directory,
// so a change to one re-pends the task. The inventory must show them or that
// re-pend has no visible cause.
func TestInventoryCoversHashIncludeTargets(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "consumer")
	writeFile(t, filepath.Join(dir, "task.yaml"), "name: consumer\n")
	writeFile(t, filepath.Join(root, "shared", "lib.js"), "export const v = 1;\n")

	files := mustInventory(t, dir, "../shared/lib.js")
	byPath := inventoryByPath(t, files)
	inc, ok := byPath["include:../shared/lib.js"]
	if !ok {
		t.Fatalf("hash_include target missing from inventory: %+v", files)
	}
	if inc.Kind != FileKindRegular {
		t.Errorf("kind: want %q, got %q", FileKindRegular, inc.Kind)
	}
	if inc.Size != int64(len("export const v = 1;\n")) {
		t.Errorf("size: got %d", inc.Size)
	}
	if len(inc.Hash) != 64 {
		t.Errorf("hash: want a 64-char sha256, got %q", inc.Hash)
	}
}

// A mistyped or deleted include changes the content hash, so it must be
// visible rather than silently absent.
func TestInventoryMarksMissingInclude(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "consumer")
	writeFile(t, filepath.Join(dir, "task.yaml"), "name: consumer\n")

	byPath := inventoryByPath(t, mustInventory(t, dir, "../shared/gone.js"))
	inc, ok := byPath["include:../shared/gone.js"]
	if !ok {
		t.Fatal("missing include absent from inventory")
	}
	if inc.Kind != FileKindMissing {
		t.Errorf("kind: want %q, got %q", FileKindMissing, inc.Kind)
	}
}

// Callers race task removal, matching Hash's own contract.
func TestInventoryMissingDirIsEmpty(t *testing.T) {
	files, err := Inventory(filepath.Join(t.TempDir(), "gone"))
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("want no entries, got %+v", files)
	}
}

func TestInventoryRejectsEscapingInclude(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "consumer")
	writeFile(t, filepath.Join(dir, "task.yaml"), "name: consumer\n")

	if _, err := Inventory(dir, "../../../../etc/passwd"); err == nil {
		t.Fatal("want an error for an include escaping the sibling boundary")
	}
}

func mustInventory(t *testing.T, dir string, includes ...string) []FileMeta {
	t.Helper()
	files, err := Inventory(dir, includes...)
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	return files
}
