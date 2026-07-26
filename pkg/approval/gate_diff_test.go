package approval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
