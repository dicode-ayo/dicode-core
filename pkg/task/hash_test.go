package task

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
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

// TestHash_ChangesOn_TsEdit was a regression gate for #157 and is now
// live after pkg/task/hash.go was extended to fold task.ts into the
// digest. Editing an existing Deno task's task.ts must change the hash so
// the reconciler picks up the update and re-registers.
func TestHash_ChangesOn_TsEdit(t *testing.T) {
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
		t.Fatalf("Hash ignored task.ts edit: before == after == %q", before)
	}
}

// TestHash_ChangesOn_MjsEdit pairs with the .ts test for the other ESM
// extension ScriptPath resolves. Regression gate against the same class
// of "allowlist forgets an extension" bug.
func TestHash_ChangesOn_MjsEdit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hello")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.yaml"), []byte("name: hello\n"), 0644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.mjs"), []byte("v1"), 0644); err != nil {
		t.Fatalf("write mjs v1: %v", err)
	}
	before, err := Hash(dir)
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.mjs"), []byte("v2"), 0644); err != nil {
		t.Fatalf("write mjs v2: %v", err)
	}
	after, err := Hash(dir)
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if before == after {
		t.Fatalf("Hash ignored task.mjs edit: before == after == %q", before)
	}
}

// TestHash_ChangesOnSubprocessScriptEdit covers the subprocess-runtime
// extensions (task.py et al.) that ScriptPath resolves — same allowlist
// invariant as the .ts/.mjs tests above.
func TestHash_ChangesOnSubprocessScriptEdit(t *testing.T) {
	for _, name := range []string{"task.py", "task.sh", "task.rb", "task.jl"} {
		dir := filepath.Join(t.TempDir(), "hello")
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "task.yaml"), []byte("name: hello\n"), 0644); err != nil {
			t.Fatalf("write yaml: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("v1"), 0644); err != nil {
			t.Fatalf("write %s v1: %v", name, err)
		}
		before, err := Hash(dir)
		if err != nil {
			t.Fatalf("before: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("v2"), 0644); err != nil {
			t.Fatalf("write %s v2: %v", name, err)
		}
		after, err := Hash(dir)
		if err != nil {
			t.Fatalf("after: %v", err)
		}
		if before == after {
			t.Fatalf("Hash ignored %s edit: before == after == %q", name, before)
		}
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

// TestHash_ChangesOnHelperFileEdit pins the whole-tree property: the Deno
// sandbox allow-reads the entire task dir, so an imported helper module
// (e.g. lib/payload.ts) is executable content. Editing ONLY the helper —
// task.yaml and task.ts byte-identical — must change the hash, otherwise
// the approval gate's lock match keeps the changed code approved.
func TestHash_ChangesOnHelperFileEdit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hello")
	writeTaskFiles(t, dir, "name: hello\n", "import './lib/x.ts'\n")
	lib := filepath.Join(dir, "lib")
	if err := os.MkdirAll(lib, 0755); err != nil {
		t.Fatalf("mkdir lib: %v", err)
	}
	if err := os.WriteFile(filepath.Join(lib, "x.ts"), []byte("export const v = 1\n"), 0644); err != nil {
		t.Fatalf("write lib/x.ts: %v", err)
	}

	before, err := Hash(dir)
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	if err := os.WriteFile(filepath.Join(lib, "x.ts"), []byte("export const v = 2\n"), 0644); err != nil {
		t.Fatalf("rewrite lib/x.ts: %v", err)
	}
	after, err := Hash(dir)
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if before == after {
		t.Fatalf("Hash ignored helper-file edit: before == after == %q", before)
	}
}

// TestHash_ChangesOnNewFile: adding any file under the task dir must change
// the hash (it becomes importable the moment it exists).
func TestHash_ChangesOnNewFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hello")
	writeTaskFiles(t, dir, "name: hello\n", "export default () => 1\n")

	before, err := Hash(dir)
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "extra.ts"), []byte("export const x = 1\n"), 0644); err != nil {
		t.Fatalf("write extra.ts: %v", err)
	}
	after, err := Hash(dir)
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if before == after {
		t.Fatalf("Hash ignored added file: before == after == %q", before)
	}
}

// TestHash_DeterministicWithNestedTree: repeated hashing of a multi-level
// tree must be stable — the reconciler diffs Hash output every poll tick.
func TestHash_DeterministicWithNestedTree(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hello")
	writeTaskFiles(t, dir, "name: hello\n", "export default () => 1\n")
	for _, p := range []string{"lib/a.ts", "lib/sub/b.ts", "vendor/z.ts", "task.test.ts"} {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(full, []byte("// "+p+"\n"), 0644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	first, err := Hash(dir)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	for i := 1; i < 10; i++ {
		got, err := Hash(dir)
		if err != nil {
			t.Fatalf("Hash trial %d: %v", i, err)
		}
		if got != first {
			t.Fatalf("Hash non-deterministic on trial %d: got %q, want %q", i, got, first)
		}
	}
}

// TestHash_SymlinkTargetStringHashed_NotFollowed: a symlink contributes its
// target STRING, never the target's bytes. Retargeting the link must change
// the hash; editing the (out-of-dir) target's content must not, because the
// hash never reads through the link.
func TestHash_SymlinkTargetStringHashed_NotFollowed(t *testing.T) {
	outside := t.TempDir()
	tgtA := filepath.Join(outside, "a.txt")
	tgtB := filepath.Join(outside, "b.txt")
	if err := os.WriteFile(tgtA, []byte("A"), 0644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := os.WriteFile(tgtB, []byte("B"), 0644); err != nil {
		t.Fatalf("write b: %v", err)
	}

	dir := filepath.Join(t.TempDir(), "hello")
	writeTaskFiles(t, dir, "name: hello\n", "export default () => 1\n")
	link := filepath.Join(dir, "data.txt")
	if err := os.Symlink(tgtA, link); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	h1, err := Hash(dir)
	if err != nil {
		t.Fatalf("h1: %v", err)
	}

	// Editing the out-of-dir target must NOT change the hash.
	if err := os.WriteFile(tgtA, []byte("A-modified"), 0644); err != nil {
		t.Fatalf("rewrite a: %v", err)
	}
	h2, err := Hash(dir)
	if err != nil {
		t.Fatalf("h2: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("Hash read through an out-of-dir symlink: %q != %q", h1, h2)
	}

	// Retargeting the link MUST change the hash.
	if err := os.Remove(link); err != nil {
		t.Fatalf("rm link: %v", err)
	}
	if err := os.Symlink(tgtB, link); err != nil {
		t.Fatalf("relink: %v", err)
	}
	h3, err := Hash(dir)
	if err != nil {
		t.Fatalf("h3: %v", err)
	}
	if h3 == h1 {
		t.Fatalf("Hash ignored symlink retarget: both = %q", h1)
	}
}

// TestHash_DanglingSymlinkTolerated: a link to a nonexistent path must not
// error (the target string is hashed, never opened).
func TestHash_DanglingSymlinkTolerated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hello")
	writeTaskFiles(t, dir, "name: hello\n", "export default () => 1\n")
	if err := os.Symlink("/nonexistent/nowhere", filepath.Join(dir, "dangling")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	if _, err := Hash(dir); err != nil {
		t.Fatalf("Hash errored on dangling symlink: %v", err)
	}
}

// TestHash_SymlinkVsRegularFileNoCollision: a regular file containing the
// link-target string must not hash the same as a symlink to that target.
func TestHash_SymlinkVsRegularFileNoCollision(t *testing.T) {
	target := "/tmp/some-target"

	a := filepath.Join(t.TempDir(), "a")
	writeTaskFiles(t, a, "name: x\n", "js")
	if err := os.Symlink(target, filepath.Join(a, "entry")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	b := filepath.Join(t.TempDir(), "b")
	writeTaskFiles(t, b, "name: x\n", "js")
	if err := os.WriteFile(filepath.Join(b, "entry"), []byte(target), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	ha, err := Hash(a)
	if err != nil {
		t.Fatalf("hash a: %v", err)
	}
	hb, err := Hash(b)
	if err != nil {
		t.Fatalf("hash b: %v", err)
	}
	if ha == hb {
		t.Fatalf("symlink and regular file collided: %q", ha)
	}
}

// TestHash_MissingDirHashesEmpty preserves the pre-rewrite behavior callers
// rely on: a dir that vanished (task removal racing a poll) hashes like an
// empty dir instead of erroring.
func TestHash_MissingDirHashesEmpty(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "never-created")
	got, err := Hash(missing)
	if err != nil {
		t.Fatalf("Hash on missing dir: %v", err)
	}
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.MkdirAll(empty, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	want, err := Hash(empty)
	if err != nil {
		t.Fatalf("Hash on empty dir: %v", err)
	}
	if got != want {
		t.Fatalf("missing dir hash %q != empty dir hash %q", got, want)
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

// TestHash_LargeFileHashedByDescriptor covers the per-file size cap: a file
// over maxHashedFileBytes folds in only its size+mtime, so rewriting its
// contents while preserving size and mtime must not change the hash, but
// changing its size must. This keeps the ~30s poll off re-reading bulk assets
// while still catching the kind of change a committed asset actually undergoes.
func TestHash_LargeFileHashedByDescriptor(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hello")
	writeTaskFiles(t, dir, "name: hello\n", "export default () => 1\n")

	asset := filepath.Join(dir, "asset.bin")
	big := bytes.Repeat([]byte{'a'}, maxHashedFileBytes+1)
	if err := os.WriteFile(asset, big, 0644); err != nil {
		t.Fatalf("write asset: %v", err)
	}
	// Pin mtime so we control the descriptor independently of write time.
	mtime := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(asset, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	before, err := Hash(dir)
	if err != nil {
		t.Fatalf("before: %v", err)
	}

	// Same size + same mtime, different bytes → descriptor unchanged → hash unchanged.
	changed := bytes.Repeat([]byte{'b'}, maxHashedFileBytes+1)
	if err := os.WriteFile(asset, changed, 0644); err != nil {
		t.Fatalf("rewrite asset: %v", err)
	}
	if err := os.Chtimes(asset, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	sameSize, err := Hash(dir)
	if err != nil {
		t.Fatalf("sameSize: %v", err)
	}
	if sameSize != before {
		t.Fatalf("hash changed for large file content edit at fixed size+mtime: %q -> %q", before, sameSize)
	}

	// Grow the file → descriptor size changes → hash changes.
	grown := bytes.Repeat([]byte{'b'}, maxHashedFileBytes+2)
	if err := os.WriteFile(asset, grown, 0644); err != nil {
		t.Fatalf("grow asset: %v", err)
	}
	if err := os.Chtimes(asset, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	grownHash, err := Hash(dir)
	if err != nil {
		t.Fatalf("grownHash: %v", err)
	}
	if grownHash == before {
		t.Fatalf("hash did not change after large file size change: both = %q", before)
	}
}

// TestHash_SmallFileEditChanges guards that the size cap does not weaken
// change-detection for code: editing a sub-threshold file (the common case)
// still changes the hash so the reconciler reloads the task.
func TestHash_SmallFileEditChanges(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hello")
	writeTaskFiles(t, dir, "name: hello\n", "export default () => 1\n")

	helper := filepath.Join(dir, "helper.js")
	if err := os.WriteFile(helper, []byte("export const x = 1\n"), 0644); err != nil {
		t.Fatalf("write helper: %v", err)
	}

	before, err := Hash(dir)
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	if err := os.WriteFile(helper, []byte("export const x = 2\n"), 0644); err != nil {
		t.Fatalf("rewrite helper: %v", err)
	}
	after, err := Hash(dir)
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if before == after {
		t.Fatalf("hash did not change after small file edit: both = %q", before)
	}
}

// TestHash_SkipsHeavyDirs covers the heavy-dir skip: files under node_modules
// and .git are not folded into the digest, so committing or churning them does
// not force a task reload nor cost the walk a full subtree read.
func TestHash_SkipsHeavyDirs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hello")
	writeTaskFiles(t, dir, "name: hello\n", "export default () => 1\n")

	before, err := Hash(dir)
	if err != nil {
		t.Fatalf("before: %v", err)
	}

	for _, sub := range []string{"node_modules", ".git"} {
		nested := filepath.Join(dir, sub, "pkg")
		if err := os.MkdirAll(nested, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", sub, err)
		}
		if err := os.WriteFile(filepath.Join(nested, "index.js"), []byte("whatever\n"), 0644); err != nil {
			t.Fatalf("write under %s: %v", sub, err)
		}
	}

	after, err := Hash(dir)
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if before != after {
		t.Fatalf("hash changed after adding files under heavy dirs: %q -> %q", before, after)
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
