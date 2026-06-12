package approval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadLockMissingFile(t *testing.T) {
	l, err := LoadLock(filepath.Join(t.TempDir(), LockFileName))
	if err != nil {
		t.Fatalf("LoadLock: %v", err)
	}
	if len(l.List()) != 0 {
		t.Fatalf("expected empty lock, got %v", l.List())
	}
}

func TestLockRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFileName)
	l, err := LoadLock(path)
	if err != nil {
		t.Fatalf("LoadLock: %v", err)
	}
	if err := l.Record("repo/deploy", "abc123", ApprovedByManual); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := l.Record("buildin/mcp", "def456", ApprovedByBuiltin); err != nil {
		t.Fatalf("Record: %v", err)
	}

	reloaded, err := LoadLock(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	rec, ok := reloaded.Get("repo/deploy")
	if !ok || rec.Hash != "abc123" || rec.ApprovedBy != ApprovedByManual {
		t.Fatalf("repo/deploy record = %+v, ok=%v", rec, ok)
	}
	if rec.ApprovedAt.IsZero() {
		t.Fatal("approved_at not persisted")
	}
	if !reloaded.Approved("repo/deploy", "abc123") {
		t.Fatal("Approved(matching hash) = false")
	}
	if reloaded.Approved("repo/deploy", "other") {
		t.Fatal("Approved(mismatched hash) = true")
	}
	if reloaded.Approved("unknown/task", "abc123") {
		t.Fatal("Approved(unknown task) = true")
	}
}

func TestLockApprovedEmptyHashNeverMatches(t *testing.T) {
	l, _ := LoadLock(filepath.Join(t.TempDir(), LockFileName))
	if l.Approved("any/task", "") {
		t.Fatal("Approved with empty hash must be false")
	}
}

func TestLockRecordSameHashKeepsOriginal(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFileName)
	l, _ := LoadLock(path)
	if err := l.Record("repo/deploy", "abc", ApprovedByManual); err != nil {
		t.Fatalf("Record: %v", err)
	}
	first, _ := l.Get("repo/deploy")
	if err := l.Record("repo/deploy", "abc", ApprovedByTrustedSource); err != nil {
		t.Fatalf("Record same hash: %v", err)
	}
	second, _ := l.Get("repo/deploy")
	if second != first {
		t.Fatalf("same-hash re-record changed entry: %+v → %+v", first, second)
	}
}

func TestLockRecordEmptyHashRejected(t *testing.T) {
	l, _ := LoadLock(filepath.Join(t.TempDir(), LockFileName))
	if err := l.Record("repo/deploy", "", ApprovedByManual); err == nil {
		t.Fatal("expected error recording empty hash")
	}
}

func TestLockRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFileName)
	l, _ := LoadLock(path)
	if err := l.Record("repo/deploy", "abc", ApprovedByManual); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := l.Remove("repo/deploy"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	reloaded, err := LoadLock(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok := reloaded.Get("repo/deploy"); ok {
		t.Fatal("removed entry survived reload")
	}
	// Removing an absent id is a no-op.
	if err := l.Remove("never/registered"); err != nil {
		t.Fatalf("Remove absent: %v", err)
	}
}

func TestLockFileHasHeaderComment(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFileName)
	l, _ := LoadLock(path)
	if err := l.Record("repo/deploy", "abc", ApprovedByManual); err != nil {
		t.Fatalf("Record: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read lock: %v", err)
	}
	if !strings.HasPrefix(string(data), "# dicode.lock") {
		t.Fatalf("lockfile missing header comment, starts with: %.60s", data)
	}
}
