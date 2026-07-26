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
// into Gate.approvedFiles/pendingFiles and out through Gate.Diff's
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
