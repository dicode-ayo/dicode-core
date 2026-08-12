package approval

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// preCommitRecord is Record as it was before Commit existed. The v3 MAC covers
// the canonical JSON of the whole task map, so this is the exact shape whose
// bytes every lock in the field was signed over.
type preCommitRecord struct {
	Hash       string    `json:"hash"`
	ApprovedAt time.Time `json:"approved_at"`
	ApprovedBy string    `json:"approved_by"`
}

type preCommitPayload struct {
	Bootstrapped bool                       `json:"bootstrapped"`
	Tasks        map[string]preCommitRecord `json:"tasks"`
}

// writePreCommitLock writes a v3 lock file in the pre-Commit format, signed
// over the pre-Commit MAC payload.
func writePreCommitLock(t *testing.T, path string, key []byte) {
	t.Helper()
	payload := preCommitPayload{
		Tasks: map[string]preCommitRecord{
			"repo/deploy": {
				Hash:       "abc123",
				ApprovedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
				ApprovedBy: ApprovedByManual,
			},
		},
	}
	canonical, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal pre-commit payload: %v", err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(canonical)

	raw := fmt.Sprintf(`version: 3
mac: %s
tasks:
    repo/deploy:
        hash: abc123
        approved_at: 2024-01-01T00:00:00Z
        approved_by: manual
`, hex.EncodeToString(mac.Sum(nil)))
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}
}

// TestLoadSignedLock_PreCommitFieldStillVerifies pins the migration: adding
// Commit must leave a record written before the field existed marshalling to
// identical bytes. Without `omitempty` the MAC payload gains `"commit":""`,
// every lock in the field verifies as tampered, and every task is forced back
// through the gate.
func TestLoadSignedLock_PreCommitFieldStillVerifies(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFileName)
	key := testSigningKey()
	writePreCommitLock(t, path, key)

	l, err := LoadSignedLock(path, key)
	if err != nil {
		t.Fatalf("LoadSignedLock: %v", err)
	}
	if l.Tampered() {
		t.Fatal("a lock written before the commit field existed must still verify")
	}
	rec, ok := l.Get("repo/deploy")
	if !ok {
		t.Fatal("record discarded")
	}
	if rec.Hash != "abc123" || rec.ApprovedBy != ApprovedByManual {
		t.Fatalf("record not intact: %+v", rec)
	}
	if rec.Commit != "" {
		t.Fatalf("commit should be empty for a pre-migration record, got %q", rec.Commit)
	}
	if !l.Approved("repo/deploy", "abc123") {
		t.Fatal("pre-migration record must still count as approved")
	}
}

// TestLoadSignedLock_PreCommitRecordSurvivesRewrite covers the second half of
// the migration: an untouched pre-migration record must keep verifying after
// the lock is rewritten for some other task.
func TestLoadSignedLock_PreCommitRecordSurvivesRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFileName)
	key := testSigningKey()
	writePreCommitLock(t, path, key)

	l, err := LoadSignedLock(path, key)
	if err != nil {
		t.Fatalf("LoadSignedLock: %v", err)
	}
	if err := l.Record("repo/other", "def456", ApprovedByManual, fakeCommit("a")); err != nil {
		t.Fatalf("Record: %v", err)
	}

	reloaded, err := LoadSignedLock(path, key)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Tampered() {
		t.Fatal("rewritten lock must verify")
	}
	old, ok := reloaded.Get("repo/deploy")
	if !ok || old.Commit != "" {
		t.Fatalf("pre-migration record changed: %+v ok=%v", old, ok)
	}
	fresh, ok := reloaded.Get("repo/other")
	if !ok || fresh.Commit != fakeCommit("a") {
		t.Fatalf("commit not persisted: %+v ok=%v", fresh, ok)
	}
}

func TestLockRecord_CommitRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFileName)
	key := testSigningKey()
	l, err := LoadSignedLock(path, key)
	if err != nil {
		t.Fatalf("LoadSignedLock: %v", err)
	}
	sha := fakeCommit("b")
	if err := l.Record("repo/deploy", "abc123", ApprovedByManual, sha); err != nil {
		t.Fatalf("Record: %v", err)
	}

	reloaded, err := LoadSignedLock(path, key)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Tampered() {
		t.Fatal("lock written with a commit must verify")
	}
	rec, ok := reloaded.Get("repo/deploy")
	if !ok || rec.Commit != sha {
		t.Fatalf("commit round-trip failed: %+v ok=%v", rec, ok)
	}
}

// TestLockRecord_UnchangedHashKeepsRecord pins that the no-op guard is keyed on
// the hash alone: a repeat record at the same hash must not rewrite the file,
// and so must not reset approved_at even when the commit has moved on.
func TestLockRecord_UnchangedHashKeepsRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFileName)
	l, err := LoadLock(path)
	if err != nil {
		t.Fatalf("LoadLock: %v", err)
	}
	first := fakeCommit("c")
	if err := l.Record("repo/deploy", "abc123", ApprovedByManual, first); err != nil {
		t.Fatalf("Record: %v", err)
	}
	before, _ := l.Get("repo/deploy")

	if err := l.Record("repo/deploy", "abc123", ApprovedByManual, fakeCommit("d")); err != nil {
		t.Fatalf("Record (repeat): %v", err)
	}
	after, _ := l.Get("repo/deploy")
	if after != before {
		t.Fatalf("repeat record at an unchanged hash rewrote the entry: %+v -> %+v", before, after)
	}
}

// fakeCommit returns a 40-character stand-in for a commit SHA.
func fakeCommit(c string) string {
	out := ""
	for len(out) < 40 {
		out += c
	}
	return out[:40]
}
