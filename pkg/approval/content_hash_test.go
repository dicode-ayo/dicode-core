package approval

import (
	"testing"

	"github.com/dicode/dicode/pkg/task"
)

// specInDir returns a dir-backed spec for the given task dir (created once by
// the caller via writeTaskDir) with the supplied mutation applied, so tests
// can vary single resolved fields against an identical on-disk dir.
func specVariant(base *task.Spec, mutate func(*task.Spec)) *task.Spec {
	s := &task.Spec{ID: base.ID, TaskDir: base.TaskDir}
	if mutate != nil {
		mutate(s)
	}
	return s
}

func mustHash(t *testing.T, k task.Kinded) string {
	t.Helper()
	h, err := ContentHash(k)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if h == "" {
		t.Fatal("ContentHash returned empty hash")
	}
	return h
}

func TestContentHashStableAcrossCalls(t *testing.T) {
	base := writeTaskDir(t, t.TempDir(), "repo/deploy", "x")
	spec := specVariant(base, func(s *task.Spec) {
		s.Runtime = task.RuntimeDeno
		s.Permissions.Net = []string{"api.github.com"}
		s.Permissions.Dicode = &task.DicodePermissions{Tasks: []string{"repo/other"}}
		s.Trigger.WebhookAuth = true
	})
	h1 := mustHash(t, spec)
	h2 := mustHash(t, spec)
	if h1 != h2 {
		t.Fatalf("ContentHash not stable: %q vs %q", h1, h2)
	}
}

// TestContentHashFoldsResolvedSecurityFields is the core of issue #400:
// taskset overrides mutate the resolved spec outside the task dir, so any
// security-bearing resolved field must perturb the hash even when the dir is
// byte-identical.
func TestContentHashFoldsResolvedSecurityFields(t *testing.T) {
	base := writeTaskDir(t, t.TempDir(), "repo/deploy", "x")

	variants := map[string]func(*task.Spec){
		"net wildcard":     func(s *task.Spec) { s.Permissions.Net = []string{"*"} },
		"fs write grant":   func(s *task.Spec) { s.Permissions.FS = []task.FSEntry{{Path: "/etc", Permission: "w"}} },
		"dicode tasks":     func(s *task.Spec) { s.Permissions.Dicode = &task.DicodePermissions{Tasks: []string{"*"}} },
		"runtime swap":     func(s *task.Spec) { s.Runtime = task.Runtime("python") },
		"webhook auth off": func(s *task.Spec) { s.Trigger.WebhookAuth = true },
	}

	baseHash := mustHash(t, specVariant(base, nil))
	for name, mutate := range variants {
		got := mustHash(t, specVariant(base, mutate))
		if got == baseHash {
			t.Errorf("%s: hash unchanged despite elevated resolved field", name)
		}
	}
}

// TestContentHashIgnoresCosmeticResolvedFields documents the intended scope:
// only security-bearing resolved fields are folded in; cosmetic resolved
// drift (e.g. an override-free description tweak applied post-load) must not
// churn approvals for dir-backed tasks.
func TestContentHashIgnoresCosmeticResolvedFields(t *testing.T) {
	base := writeTaskDir(t, t.TempDir(), "repo/deploy", "x")
	plain := mustHash(t, specVariant(base, nil))
	cosmetic := mustHash(t, specVariant(base, func(s *task.Spec) {
		s.Description = "totally different description"
		s.Name = "renamed"
	}))
	if plain != cosmetic {
		t.Fatalf("cosmetic field change altered hash: %q vs %q", plain, cosmetic)
	}
}

// TestContentHashPipelineTaskKeepsDirHash: pipelines are not subject to
// permission-replacing taskset overrides, so their gate hash remains the
// plain directory hash.
func TestContentHashPipelineTaskKeepsDirHash(t *testing.T) {
	// Reuse writeTaskDir for the directory contents; only the dir matters.
	dir := writeTaskDir(t, t.TempDir(), "repo/pipe", "stages").TaskDir
	p := &task.PipelineTask{ID: "repo/pipe", TaskDir: dir}
	got := mustHash(t, p)
	want, err := task.Hash(dir)
	if err != nil {
		t.Fatalf("task.Hash: %v", err)
	}
	if got != want {
		t.Fatalf("pipeline ContentHash = %q, want plain dir hash %q", got, want)
	}
}

// TestOverrideElevatedPermissionsRePend is the gate-level regression test for
// issue #400: an approved task whose resolved permissions are later elevated
// by a taskset override (same dir on disk) must be held pending again, not
// auto-approved off the stale lock entry.
func TestOverrideElevatedPermissionsRePend(t *testing.T) {
	g, arm, lock := newTestGate(t, enabledPolicy())
	base := writeTaskDir(t, t.TempDir(), "repo/deploy", "v1")

	// Admit at base permissions and approve.
	if armed, err := g.Admit(specVariant(base, nil)); err != nil || armed {
		t.Fatalf("Admit base = (%v, %v), want pending", armed, err)
	}
	if err := g.Approve("repo/deploy"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	approvedRec, _ := lock.Get("repo/deploy")

	// Same dir, but the resolved spec now carries override-elevated
	// permissions (simulating a taskset.yaml edit outside the task dir).
	elevated := specVariant(base, func(s *task.Spec) {
		s.Permissions.Net = []string{"*"}
		s.Permissions.Dicode = &task.DicodePermissions{SecretsWrite: true}
	})
	armed, err := g.Admit(elevated)
	if err != nil {
		t.Fatalf("Admit elevated: %v", err)
	}
	if armed {
		t.Fatal("override-elevated task must re-pend, got armed (issue #400 bypass)")
	}
	if !g.IsPending("repo/deploy") {
		t.Fatal("elevated task not in pending set")
	}
	if got := arm.armedIDs(); len(got) != 1 {
		t.Fatalf("armed = %v, want only the original approval", got)
	}
	// The lock keeps the previously approved hash for drift inspection.
	if rec, ok := lock.Get("repo/deploy"); !ok || rec != approvedRec {
		t.Fatalf("lock record changed on re-pend: %+v → %+v (ok=%v)", approvedRec, rec, ok)
	}
}
