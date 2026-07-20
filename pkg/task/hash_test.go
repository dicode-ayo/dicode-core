package task

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
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

// ── hash_include (#585) ─────────────────────────────────────────────────

// TestHash_IncludeFileEditChangesHash is the core regression gate for #585:
// a shared module living outside the task dir must perturb the hash when
// edited, as long as it's named in includes — otherwise the approval gate
// never re-pends importers of a changed shared module.
func TestHash_IncludeFileEditChangesHash(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared", "chat.ts")
	if err := os.MkdirAll(filepath.Dir(shared), 0755); err != nil {
		t.Fatalf("mkdir shared: %v", err)
	}
	if err := os.WriteFile(shared, []byte("export const v = 1\n"), 0644); err != nil {
		t.Fatalf("write shared v1: %v", err)
	}

	dir := filepath.Join(root, "task-a")
	writeTaskFiles(t, dir, "name: a\nhash_include: [\"../shared/chat.ts\"]\n", "import '../shared/chat.ts'\n")

	before, err := Hash(dir, "../shared/chat.ts")
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	if err := os.WriteFile(shared, []byte("export const v = 2\n"), 0644); err != nil {
		t.Fatalf("rewrite shared: %v", err)
	}
	after, err := Hash(dir, "../shared/chat.ts")
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if before == after {
		t.Fatalf("Hash ignored an edit to an included shared module: before == after == %q", before)
	}
}

// TestHash_WithoutInclude_SharedModuleEditInvisible is the control for the
// test above: without hash_include, the same edit to the same shared module
// must NOT change the hash — this is the pre-#585 hole the feature closes,
// pinned here so a future refactor can't silently make Hash always walk
// outward (which would break the "sandbox only reads TaskDir" invariant).
func TestHash_WithoutInclude_SharedModuleEditInvisible(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared", "chat.ts")
	if err := os.MkdirAll(filepath.Dir(shared), 0755); err != nil {
		t.Fatalf("mkdir shared: %v", err)
	}
	if err := os.WriteFile(shared, []byte("export const v = 1\n"), 0644); err != nil {
		t.Fatalf("write shared v1: %v", err)
	}

	dir := filepath.Join(root, "task-a")
	writeTaskFiles(t, dir, "name: a\n", "import '../shared/chat.ts'\n")

	before, err := Hash(dir) // no includes
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	if err := os.WriteFile(shared, []byte("export const v = 2\n"), 0644); err != nil {
		t.Fatalf("rewrite shared: %v", err)
	}
	after, err := Hash(dir)
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if before != after {
		t.Fatalf("Hash without includes unexpectedly saw an out-of-dir edit: %q -> %q", before, after)
	}
}

// TestHash_IncludeDirWalkedRecursively covers a directory include: every
// file under it participates in the digest, not just the top level.
func TestHash_IncludeDirWalkedRecursively(t *testing.T) {
	root := t.TempDir()
	sharedDir := filepath.Join(root, "shared")
	nested := filepath.Join(sharedDir, "nested")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nested, "deep.ts"), []byte("v1"), 0644); err != nil {
		t.Fatalf("write deep.ts: %v", err)
	}

	dir := filepath.Join(root, "task-a")
	writeTaskFiles(t, dir, "name: a\n", "js")

	before, err := Hash(dir, "../shared")
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nested, "deep.ts"), []byte("v2"), 0644); err != nil {
		t.Fatalf("rewrite deep.ts: %v", err)
	}
	after, err := Hash(dir, "../shared")
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if before == after {
		t.Fatalf("Hash ignored an edit nested inside a directory include: before == after == %q", before)
	}
}

// TestHash_MissingIncludeFoldsSentinel_NotError: a hash_include path that
// doesn't exist (deleted shared module, typo) must not error the hash — the
// reconciler runs Hash on every poll and a hard error there would break
// change-detection for the whole task — but it must still perturb the
// digest relative to "include not configured at all", so a deleted include
// is itself a detectable change rather than a silent no-op.
func TestHash_MissingIncludeFoldsSentinel_NotError(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "task-a")
	writeTaskFiles(t, dir, "name: a\n", "js")

	withMissingInclude, err := Hash(dir, "../nonexistent/module.ts")
	if err != nil {
		t.Fatalf("Hash errored on a missing include: %v", err)
	}
	withoutInclude, err := Hash(dir)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if withMissingInclude == withoutInclude {
		t.Fatalf("a missing include produced the same hash as no include at all: %q", withMissingInclude)
	}
}

// TestHash_IncludeLabelDoesNotCollideWithInDirFile: an include path and an
// in-dir file of the same base name must not be hashed as the same digest
// contribution — the "include:" label prefix exists precisely to keep these
// two namespaces distinct (see the Hash doc comment).
func TestHash_IncludeLabelDoesNotCollideWithInDirFile(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared.ts")
	if err := os.WriteFile(shared, []byte("shared-content"), 0644); err != nil {
		t.Fatalf("write shared: %v", err)
	}

	// Task with an in-dir file literally named "shared.ts" plus an include of
	// the same base name pointing outside — these must be treated as two
	// independent entries in the digest, not merged/aliased.
	dir := filepath.Join(root, "task-a")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "shared.ts"), []byte("in-dir-content"), 0644); err != nil {
		t.Fatalf("write in-dir shared.ts: %v", err)
	}

	withInclude, err := Hash(dir, "../shared.ts")
	if err != nil {
		t.Fatalf("withInclude: %v", err)
	}
	withoutInclude, err := Hash(dir)
	if err != nil {
		t.Fatalf("withoutInclude: %v", err)
	}
	if withInclude == withoutInclude {
		t.Fatalf("including an out-of-dir file with the same base name as an in-dir file was a no-op: %q", withInclude)
	}
}

// TestHash_IncludeOrderIndependent: includes are sorted before hashing, so
// declaring them in a different order in task.yaml must not change the hash
// — otherwise reordering an unrelated list would spuriously re-pend approval.
func TestHash_IncludeOrderIndependent(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"one.ts", "two.ts"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	dir := filepath.Join(root, "task-a")
	writeTaskFiles(t, dir, "name: a\n", "js")

	forward, err := Hash(dir, "../one.ts", "../two.ts")
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	reverse, err := Hash(dir, "../two.ts", "../one.ts")
	if err != nil {
		t.Fatalf("reverse: %v", err)
	}
	if forward != reverse {
		t.Fatalf("include order changed the hash: %q vs %q", forward, reverse)
	}
}

// TestHash_IncludeEscapingSiblingScopeIsRejected is the path-traversal
// regression: hash_include must not be able to walk arbitrarily far up the
// filesystem to read host files unrelated to the taskset (e.g. "/etc/passwd"
// via enough "../" hops) — it is bounded to dir's parent directory (the
// sibling-task scope the feature exists for). Reported by CodeQL as
// "Uncontrolled data used in path expression" against the pre-fix code.
func TestHash_IncludeEscapingSiblingScopeIsRejected(t *testing.T) {
	// tasksRoot/task-a is the task dir; tasksRoot/../outside sits one level
	// above tasksRoot itself, i.e. two hops up from task-a — outside the
	// sibling-task boundary (tasksRoot).
	parent := t.TempDir()
	tasksRoot := filepath.Join(parent, "tasks-root")
	outside := filepath.Join(parent, "outside")
	if err := os.MkdirAll(outside, 0755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("host-secret"), 0644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	dir := filepath.Join(tasksRoot, "task-a")
	writeTaskFiles(t, dir, "name: a\n", "js")

	_, err := Hash(dir, "../../outside/secret.txt")
	if err == nil {
		t.Fatal("Hash allowed a hash_include entry to escape the sibling-task boundary — path traversal not rejected")
	}
}

// TestHash_IncludeWithinSiblingScopeStillAllowed is the control for the test
// above: a "../" that stays within the sibling-task boundary (one hop up,
// into a sibling of dir) must still be accepted — this is the feature's
// entire purpose (a sibling buildin task's shared helper library).
func TestHash_IncludeWithinSiblingScopeStillAllowed(t *testing.T) {
	tasksRoot := t.TempDir()
	sibling := filepath.Join(tasksRoot, "sibling.ts")
	if err := os.WriteFile(sibling, []byte("shared"), 0644); err != nil {
		t.Fatalf("write sibling: %v", err)
	}

	dir := filepath.Join(tasksRoot, "task-a")
	writeTaskFiles(t, dir, "name: a\n", "js")

	if _, err := Hash(dir, "../sibling.ts"); err != nil {
		t.Fatalf("Hash rejected an in-bounds sibling include: %v", err)
	}
}

// TestHash_IncludeExactlyTheBoundaryIsRejected is the regression for a
// critical off-by-one caught in review: hash_include: [".."] resolves to
// EXACTLY dir's parent directory (the boundary itself), which the original
// "rel == '..' or has prefix '../'" check did not reject (rel == "." in that
// case). Left unfixed, this would let a single hash_include entry pull the
// ENTIRE taskset root — every sibling task, not just one — into this task's
// content hash, defeating the whole point of scoping to "sibling task".
func TestHash_IncludeExactlyTheBoundaryIsRejected(t *testing.T) {
	tasksRoot := t.TempDir()
	dirA := filepath.Join(tasksRoot, "task-a")
	writeTaskFiles(t, dirA, "name: a\n", "js")
	dirB := filepath.Join(tasksRoot, "task-b")
	writeTaskFiles(t, dirB, "name: b\n", "js")

	if _, err := Hash(dirA, ".."); err == nil {
		t.Fatal("Hash accepted hash_include: [\"..\"] — the boundary itself, which would pull in every sibling task, not just one")
	}
}

// TestHash_IncludeThroughSymlinkedIntermediateDirIsRejected is the
// regression for the git-committed-symlink escape caught in review:
// resolveInclude's lexical containment check alone can't see that a
// directory COMPONENT of the include path is itself a symlink pointing
// outside the sibling-task boundary — go-git materializes repo-committed
// symlinks as real on-disk links, so this is reachable from an ordinary git
// source, not just a local filesystem trick. pathguard.WithinResolved must
// catch it.
func TestHash_IncludeThroughSymlinkedIntermediateDirIsRejected(t *testing.T) {
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

	// A symlinked directory INSIDE the sibling-task boundary that physically
	// redirects outside it.
	evilLink := filepath.Join(tasksRoot, "evil-link")
	if err := os.Symlink(outside, evilLink); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	dir := filepath.Join(tasksRoot, "task-a")
	writeTaskFiles(t, dir, "name: a\n", "js")

	// Lexically, "../evil-link/secret.txt" looks like it stays inside
	// tasksRoot (the boundary) — only resolving the symlink reveals it
	// actually reaches outside/secret.txt.
	if _, err := Hash(dir, "../evil-link/secret.txt"); err == nil {
		t.Fatal("Hash followed a git-committed symlink out of the sibling-task boundary — path traversal via intermediate symlink not rejected")
	}
}

// TestHash_IncludeFifoIsSkippedNotRead is the regression for a hang caught
// in review: a hash_include entry naming a non-regular, non-symlink,
// non-directory path (a FIFO here) must be silently skipped, exactly like
// walkTree already does for the same file types found during a normal dir
// walk — not read via os.ReadFile, which blocks indefinitely on a FIFO with
// no writer.
func TestHash_IncludeFifoIsSkippedNotRead(t *testing.T) {
	tasksRoot := t.TempDir()
	fifoPath := filepath.Join(tasksRoot, "a.fifo")
	if err := syscall.Mkfifo(fifoPath, 0644); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}

	dir := filepath.Join(tasksRoot, "task-a")
	writeTaskFiles(t, dir, "name: a\n", "js")

	done := make(chan struct{})
	var hash string
	var hashErr error
	go func() {
		hash, hashErr = Hash(dir, "../a.fifo")
		close(done)
	}()
	select {
	case <-done:
		if hashErr != nil {
			t.Fatalf("Hash errored on a FIFO include: %v", hashErr)
		}
		if hash == "" {
			t.Fatal("Hash returned an empty digest")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Hash blocked reading a FIFO hash_include entry instead of skipping it")
	}
}

// TestScanDir_HonorsHashInclude is the regression for a gap caught in
// review: ScanDir (the change-detection primitive for the flat local/git
// source types, which never fully load a task.Spec) must still read each
// task's hash_include list — via the lightweight readHashInclude parse —
// or hash_include silently does nothing for any task registered through
// those source types, even though the same task registered via a taskset.yaml
// source (pkg/taskset/source.go's snapHash) would correctly honor it.
func TestScanDir_HonorsHashInclude(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared.ts")
	if err := os.WriteFile(shared, []byte("v1"), 0644); err != nil {
		t.Fatalf("write shared: %v", err)
	}
	writeTaskFiles(t, filepath.Join(root, "task-a"),
		"name: a\nhash_include: [\"../shared.ts\"]\n", "js")

	before, err := ScanDir(root)
	if err != nil {
		t.Fatalf("ScanDir before: %v", err)
	}
	if err := os.WriteFile(shared, []byte("v2"), 0644); err != nil {
		t.Fatalf("rewrite shared: %v", err)
	}
	after, err := ScanDir(root)
	if err != nil {
		t.Fatalf("ScanDir after: %v", err)
	}
	if before["task-a"] == after["task-a"] {
		t.Fatalf("ScanDir ignored an edit to a hash_include'd shared module: before == after == %q", before["task-a"])
	}
}

// TestHash_IncludeSymlinkTargetContentIsHashed is the regression for a
// finding caught in review: when the hash_include entry ITSELF is a
// symlink (an alias to the real shared file), resolveInclude now resolves
// through it and hashes the REAL target's content — unlike an ordinary
// in-dir symlink (TestHash_SymlinkTargetStringHashed_NotFollowed), which
// deliberately hashes only the target string. If it didn't, editing the
// content behind an included symlink would reopen the exact gap
// hash_include exists to close (#585): the code that actually runs would
// change on next spawn without the hash ever reflecting it.
func TestHash_IncludeSymlinkTargetContentIsHashed(t *testing.T) {
	tasksRoot := t.TempDir()
	real := filepath.Join(tasksRoot, "real.ts")
	if err := os.WriteFile(real, []byte("v1"), 0644); err != nil {
		t.Fatalf("write real: %v", err)
	}
	alias := filepath.Join(tasksRoot, "alias.ts")
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	dir := filepath.Join(tasksRoot, "task-a")
	writeTaskFiles(t, dir, "name: a\n", "js")

	before, err := Hash(dir, "../alias.ts")
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	if err := os.WriteFile(real, []byte("v2"), 0644); err != nil {
		t.Fatalf("rewrite real: %v", err)
	}
	after, err := Hash(dir, "../alias.ts")
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if before == after {
		t.Fatalf("Hash ignored a content edit behind a hash_include symlink alias: before == after == %q", before)
	}
}

// TestHash_IncludeSymlinkDirTargetContentIsHashed is the directory-include
// counterpart of the test above: the include entry is a symlink TO A
// DIRECTORY, and a file edited inside the real directory (reached only
// through the symlink) must still change the hash.
func TestHash_IncludeSymlinkDirTargetContentIsHashed(t *testing.T) {
	tasksRoot := t.TempDir()
	realDir := filepath.Join(tasksRoot, "real-shared")
	if err := os.MkdirAll(realDir, 0755); err != nil {
		t.Fatalf("mkdir realDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "helper.ts"), []byte("v1"), 0644); err != nil {
		t.Fatalf("write helper: %v", err)
	}
	aliasDir := filepath.Join(tasksRoot, "alias-shared")
	if err := os.Symlink(realDir, aliasDir); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	dir := filepath.Join(tasksRoot, "task-a")
	writeTaskFiles(t, dir, "name: a\n", "js")

	before, err := Hash(dir, "../alias-shared")
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "helper.ts"), []byte("v2"), 0644); err != nil {
		t.Fatalf("rewrite helper: %v", err)
	}
	after, err := Hash(dir, "../alias-shared")
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if before == after {
		t.Fatalf("Hash ignored a content edit inside a hash_include symlinked directory: before == after == %q", before)
	}
}

// TestHash_InDirFifoIsSkipped covers walkTree's own entry classification
// (used for the main dir walk and directory includes) with a real stat
// (d.Info()) rather than the raw directory-entry type (d.Type()) — the
// latter can report 0 ("unknown", indistinguishable from "regular") on
// filesystems that don't populate d_type, which would otherwise let a FIFO
// slip through IsRegular() and into a blocking os.ReadFile. This doesn't
// reproduce the DT_UNKNOWN condition itself (filesystem-dependent, not
// under test control), but pins that a FIFO sitting directly in the walked
// dir is still skipped, not hashed as content, after the d.Info() switch.
func TestHash_InDirFifoIsSkipped(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "task-a")
	writeTaskFiles(t, dir, "name: a\n", "js")
	fifoPath := filepath.Join(dir, "a.fifo")
	if err := syscall.Mkfifo(fifoPath, 0644); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}

	done := make(chan struct{})
	var hash string
	var hashErr error
	go func() {
		hash, hashErr = Hash(dir)
		close(done)
	}()
	select {
	case <-done:
		if hashErr != nil {
			t.Fatalf("Hash errored on an in-dir FIFO: %v", hashErr)
		}
		if hash == "" {
			t.Fatal("Hash returned an empty digest")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Hash blocked reading an in-dir FIFO instead of skipping it")
	}
}
