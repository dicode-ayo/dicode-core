package approval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newTestGateWithCache is like newTestGate but wires a real on-disk snapshot
// cache directory (cacheDir), so persistence across separate Gate instances
// pointed at the same lock/cache can be exercised — simulating a daemon
// restart, where the process (and every in-memory Gate field) is gone but the
// lock file and the snapshot cache directory survive on disk.
func newTestGateWithCache(t *testing.T, policy Policy, lockPath, cacheDir string) (*Gate, *fakeArm, *Lock) {
	t.Helper()
	lock, err := LoadLock(lockPath)
	if err != nil {
		t.Fatalf("LoadLock: %v", err)
	}
	arm := &fakeArm{}
	return NewGate(policy, lock, cacheDir, arm.arm, nil), arm, lock
}

// TestApprovedSnapshotSurvivesGateRestart is the core regression for #642: a
// second Gate instance pointed at the same on-disk lock + snapshot cache as a
// first one — simulating a daemon restart, where nothing survives in memory
// but the lock file and cache directory do — must still produce a real diff
// baseline (HasBaseline: true, with correct prior content) for a task that is
// pending at a new hash, as long as that task was approved by the first Gate
// instance.
//
// Verified to genuinely fail against the pre-#642 code: with persistence
// disabled (cacheDir == ""), the second Gate has no way to recover the
// baseline and Diff reports HasBaseline: false — see
// TestApprovedSnapshotLostAcrossRestartWithoutCache below for that same
// assertion pinned as an explicit contrast case.
func TestApprovedSnapshotSurvivesGateRestart(t *testing.T) {
	root := t.TempDir()
	lockPath := filepath.Join(root, LockFileName)
	cacheDir := filepath.Join(root, "approval-snapshots")
	taskRoot := filepath.Join(root, "tasks")

	// First "process": admit, approve at v1.
	g1, _, _ := newTestGateWithCache(t, enabledPolicy(), lockPath, cacheDir)
	spec := writeTaskDir(t, taskRoot, "repo/deploy", "line one\nline two\n")
	if armed, err := g1.Admit(spec); err != nil || armed {
		t.Fatalf("Admit = (%v, %v), want (false, nil) pending", armed, err)
	}
	if err := g1.Approve("repo/deploy"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	// The task changes on disk (e.g. a git pull that landed while the daemon
	// was down) before the "process" restarts.
	if err := os.WriteFile(filepath.Join(spec.TaskDir, "task.js"), []byte("line one\nline TWO CHANGED\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Second "process": a brand new Gate, sharing only the lock file and the
	// cache directory on disk — no Admit/Approve history in memory at all.
	g2, _, _ := newTestGateWithCache(t, enabledPolicy(), lockPath, cacheDir)
	if armed, err := g2.Admit(spec); err != nil || armed {
		t.Fatalf("post-restart Admit = (%v, %v), want (false, nil) pending", armed, err)
	}

	d, err := g2.Diff("repo/deploy")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !d.HasBaseline {
		t.Fatal("HasBaseline = false after restart, want true: the persisted cache should have supplied the baseline")
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
		t.Errorf("unified diff missing removed line from the pre-restart approved content: %q", jsDiff.UnifiedDiff)
	}
	if !strings.Contains(jsDiff.UnifiedDiff, "+ line TWO CHANGED") {
		t.Errorf("unified diff missing added line: %q", jsDiff.UnifiedDiff)
	}
}

// TestApprovedSnapshotLostAcrossRestartWithoutCache pins the pre-#642
// behavior as an explicit contrast: with persistence disabled (empty
// cacheDir, same as every other test in this package via newTestGate), a
// second Gate instance genuinely has no baseline after a "restart" — this is
// what proves TestApprovedSnapshotSurvivesGateRestart is exercising the fix
// and not some other path to a baseline (e.g. re-walking the directory at the
// old hash, which the gate cannot do since the old content is gone from
// disk).
func TestApprovedSnapshotLostAcrossRestartWithoutCache(t *testing.T) {
	root := t.TempDir()
	lockPath := filepath.Join(root, LockFileName)
	taskRoot := filepath.Join(root, "tasks")

	g1, _, _ := newTestGateWithCache(t, enabledPolicy(), lockPath, "") // cache disabled
	spec := writeTaskDir(t, taskRoot, "repo/deploy", "line one\nline two\n")
	if armed, err := g1.Admit(spec); err != nil || armed {
		t.Fatalf("Admit = (%v, %v), want (false, nil) pending", armed, err)
	}
	if err := g1.Approve("repo/deploy"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if err := os.WriteFile(filepath.Join(spec.TaskDir, "task.js"), []byte("line one\nline TWO CHANGED\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	g2, _, _ := newTestGateWithCache(t, enabledPolicy(), lockPath, "") // still disabled
	if armed, err := g2.Admit(spec); err != nil || armed {
		t.Fatalf("post-restart Admit = (%v, %v), want (false, nil) pending", armed, err)
	}
	d, err := g2.Diff("repo/deploy")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if d.HasBaseline {
		t.Fatal("HasBaseline = true with persistence disabled, want false")
	}
}

// TestCachedSnapshotIgnoredWhenHashDoesNotMatchLock covers the invalidation
// rule from the issue: a cache entry is dropped/ignored whenever its hash no
// longer matches what dicode.lock currently records as approved for that
// task ID, so the cache can never go stale against the lock. Simulated
// directly by writing a cache entry that disagrees with the lock's record.
func TestCachedSnapshotIgnoredWhenHashDoesNotMatchLock(t *testing.T) {
	root := t.TempDir()
	lockPath := filepath.Join(root, LockFileName)
	cacheDir := filepath.Join(root, "approval-snapshots")
	taskRoot := filepath.Join(root, "tasks")

	g1, _, lock := newTestGateWithCache(t, enabledPolicy(), lockPath, cacheDir)
	spec := writeTaskDir(t, taskRoot, "repo/deploy", "v1\n")
	if _, err := g1.Admit(spec); err != nil {
		t.Fatal(err)
	}
	if err := g1.Approve("repo/deploy"); err != nil {
		t.Fatal(err)
	}
	rec, ok := lock.Get("repo/deploy")
	if !ok {
		t.Fatal("expected a lock record after approval")
	}

	// Corrupt the cache: write an entry whose Hash disagrees with the lock's
	// current record (as if the lock had since been updated through another
	// path, or the cache were simply stale/forged).
	cache := newSnapshotCache(cacheDir)
	if err := cache.save("repo/deploy", "not-"+rec.Hash, map[string]snapshotValue{
		"task.js": {Content: "forged content that must never surface"},
	}, ""); err != nil {
		t.Fatalf("seed forged cache entry: %v", err)
	}

	if err := os.WriteFile(filepath.Join(spec.TaskDir, "task.js"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g2, _, _ := newTestGateWithCache(t, enabledPolicy(), lockPath, cacheDir)
	if armed, err := g2.Admit(spec); err != nil || armed {
		t.Fatalf("Admit = (%v, %v), want pending", armed, err)
	}
	d, err := g2.Diff("repo/deploy")
	if err != nil {
		t.Fatal(err)
	}
	if d.HasBaseline {
		t.Fatal("HasBaseline = true from a cache entry whose hash disagrees with the lock, want false (mismatched entries must be ignored)")
	}
	for _, f := range d.Files {
		if strings.Contains(f.UnifiedDiff, "forged content") {
			t.Fatalf("forged/mismatched cache content leaked into the diff: %q", f.UnifiedDiff)
		}
	}
}

// TestForgetDeletesPersistedSnapshot covers Forget's cache cleanup: after
// Forget, a re-admit of the same task ID at a hash that happens to collide
// with the approved-and-forgotten one must not resurrect the old cache entry
// (guards the hygiene cleanup in Forget, on top of Diff's own hash check
// already covered by TestForgetDropsApprovedBaseline in gate_diff_test.go).
func TestForgetDeletesPersistedSnapshot(t *testing.T) {
	root := t.TempDir()
	lockPath := filepath.Join(root, LockFileName)
	cacheDir := filepath.Join(root, "approval-snapshots")
	taskRoot := filepath.Join(root, "tasks")

	g, _, _ := newTestGateWithCache(t, enabledPolicy(), lockPath, cacheDir)
	spec := writeTaskDir(t, taskRoot, "repo/deploy", "v1\n")
	if _, err := g.Admit(spec); err != nil {
		t.Fatal(err)
	}
	if err := g.Approve("repo/deploy"); err != nil {
		t.Fatal(err)
	}

	cache := newSnapshotCache(cacheDir)
	if _, err := os.Stat(cache.path("repo/deploy")); err != nil {
		t.Fatalf("expected a persisted cache entry after approval: %v", err)
	}

	g.Forget("repo/deploy")
	if _, err := os.Stat(cache.path("repo/deploy")); !os.IsNotExist(err) {
		t.Fatalf("Forget did not remove the persisted cache entry: stat err = %v", err)
	}
}
