package approval

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
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

// testSigningKey returns a fixed 32-byte key for use in signing tests.
func testSigningKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return key
}

func TestLoadSignedLock_MissingFile(t *testing.T) {
	l, err := LoadSignedLock(filepath.Join(t.TempDir(), LockFileName), testSigningKey())
	if err != nil {
		t.Fatalf("LoadSignedLock on missing file: %v", err)
	}
	if len(l.List()) != 0 {
		t.Fatal("expected empty lock for missing file")
	}
	if l.Tampered() {
		t.Fatal("Tampered() should be false for missing file")
	}
}

func TestLoadSignedLock_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFileName)
	key := testSigningKey()

	l, err := LoadSignedLock(path, key)
	if err != nil {
		t.Fatalf("initial LoadSignedLock: %v", err)
	}
	if err := l.Record("repo/deploy", "abc123", ApprovedByManual); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Reload and verify the MAC passes.
	reloaded, err := LoadSignedLock(path, key)
	if err != nil {
		t.Fatalf("reload LoadSignedLock: %v", err)
	}
	if reloaded.Tampered() {
		t.Fatal("Tampered() should be false after valid round-trip")
	}
	rec, ok := reloaded.Get("repo/deploy")
	if !ok || rec.Hash != "abc123" {
		t.Fatalf("expected repo/deploy hash abc123, got %+v ok=%v", rec, ok)
	}
}

func TestLoadSignedLock_LegacyUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFileName)
	key := testSigningKey()

	// Write a legacy unsigned lock (LoadLock = no key).
	l, err := LoadLock(path)
	if err != nil {
		t.Fatalf("LoadLock: %v", err)
	}
	if err := l.Record("repo/deploy", "abc123", ApprovedByManual); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Verify the file has no mac field.
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "mac:") {
		t.Fatal("legacy lock should not have mac field")
	}

	// Load with a key: should upgrade (re-sign) and succeed.
	upgraded, err := LoadSignedLock(path, key)
	if err != nil {
		t.Fatalf("LoadSignedLock on legacy file: %v", err)
	}
	if upgraded.Tampered() {
		t.Fatal("Tampered() should be false after legacy upgrade")
	}
	if _, ok := upgraded.Get("repo/deploy"); !ok {
		t.Fatal("records should survive legacy upgrade")
	}

	// Now the file should have a mac field.
	data, _ = os.ReadFile(path)
	if !strings.Contains(string(data), "mac:") {
		t.Fatal("upgraded lock should have mac field")
	}

	// A subsequent load with the same key should verify cleanly.
	verified, err := LoadSignedLock(path, key)
	if err != nil {
		t.Fatalf("second LoadSignedLock after upgrade: %v", err)
	}
	if verified.Tampered() {
		t.Fatal("Tampered() should be false after upgrade and verify")
	}
}

func TestLoadSignedLock_TamperedContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFileName)
	key := testSigningKey()

	l, err := LoadSignedLock(path, key)
	if err != nil {
		t.Fatalf("LoadSignedLock: %v", err)
	}
	if err := l.Record("repo/deploy", "abc123", ApprovedByManual); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Tamper with the file by replacing the hash.
	data, _ := os.ReadFile(path)
	tampered := strings.ReplaceAll(string(data), "abc123", "forged!")
	if err := os.WriteFile(path, []byte(tampered), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Loading with the correct key should detect the tampering.
	reloaded, err := LoadSignedLock(path, key)
	if err != nil {
		t.Fatalf("LoadSignedLock on tampered file: %v", err)
	}
	if !reloaded.Tampered() {
		t.Fatal("Tampered() should be true after content modification")
	}
	// Fail closed: records must be empty.
	if len(reloaded.List()) != 0 {
		t.Fatalf("expected empty records after tamper detection, got %v", reloaded.List())
	}
}

func TestLoadSignedLock_WrongKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFileName)
	key := testSigningKey()

	l, err := LoadSignedLock(path, key)
	if err != nil {
		t.Fatalf("LoadSignedLock: %v", err)
	}
	if err := l.Record("repo/deploy", "abc123", ApprovedByManual); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Load with a different key.
	differentKey := make([]byte, 32)
	for i := range differentKey {
		differentKey[i] = 0xFF
	}
	reloaded, err := LoadSignedLock(path, differentKey)
	if err != nil {
		t.Fatalf("LoadSignedLock with wrong key: %v", err)
	}
	if !reloaded.Tampered() {
		t.Fatal("Tampered() should be true when wrong key is used")
	}
}

func TestLoadSignedLock_UnsignedModeAcceptsSignedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFileName)
	key := testSigningKey()

	l, err := LoadSignedLock(path, key)
	if err != nil {
		t.Fatalf("LoadSignedLock: %v", err)
	}
	if err := l.Record("repo/deploy", "abc123", ApprovedByManual); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// LoadLock (unsigned mode) should still be able to read the file.
	unsigned, err := LoadLock(path)
	if err != nil {
		t.Fatalf("LoadLock on signed file: %v", err)
	}
	if unsigned.Tampered() {
		t.Fatal("Tampered() should always be false in unsigned mode")
	}
	if _, ok := unsigned.Get("repo/deploy"); !ok {
		t.Fatal("records should be readable in unsigned mode even from signed file")
	}
}

func TestLockSignedFileHasMACField(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFileName)
	l, err := LoadSignedLock(path, testSigningKey())
	if err != nil {
		t.Fatalf("LoadSignedLock: %v", err)
	}
	if err := l.Record("repo/deploy", "abc", ApprovedByManual); err != nil {
		t.Fatalf("Record: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read lock: %v", err)
	}
	if !strings.Contains(string(data), "mac:") {
		t.Fatal("signed lock should contain mac field")
	}
}

func TestLockMACIsValidHex(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFileName)
	l, err := LoadSignedLock(path, testSigningKey())
	if err != nil {
		t.Fatalf("LoadSignedLock: %v", err)
	}
	if err := l.Record("repo/deploy", "abc", ApprovedByManual); err != nil {
		t.Fatalf("Record: %v", err)
	}
	data, _ := os.ReadFile(path)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "mac:") {
			macVal := strings.TrimSpace(strings.TrimPrefix(line, "mac:"))
			if _, err := hex.DecodeString(macVal); err != nil {
				t.Fatalf("mac field is not valid hex: %q", macVal)
			}
			if len(macVal) != 64 {
				t.Fatalf("mac field should be 64 hex chars (SHA-256), got %d", len(macVal))
			}
			return
		}
	}
	t.Fatal("mac field not found in lock file")
}

func TestLoadSignedLock_UppercaseHexMAC(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFileName)
	key := testSigningKey()

	l, _ := LoadSignedLock(path, key)
	if err := l.Record("repo/deploy", "abc", ApprovedByManual); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Convert the mac field to uppercase hex.
	data, _ := os.ReadFile(path)
	modified := string(data)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "mac:") {
			macVal := strings.TrimSpace(strings.TrimPrefix(line, "mac:"))
			modified = strings.ReplaceAll(string(data), macVal, strings.ToUpper(macVal))
			break
		}
	}
	if err := os.WriteFile(path, []byte(modified), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	reloaded, err := LoadSignedLock(path, key)
	if err != nil {
		t.Fatalf("LoadSignedLock with uppercase MAC: %v", err)
	}
	if reloaded.Tampered() {
		t.Fatal("Tampered() should be false for uppercase hex MAC")
	}
}

func TestLockMarkBootstrapped_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFileName)
	key := testSigningKey()
	l, err := LoadSignedLock(path, key)
	if err != nil {
		t.Fatalf("LoadSignedLock: %v", err)
	}
	if l.IsBootstrapped() {
		t.Fatal("IsBootstrapped() should be false on fresh lock")
	}
	if err := l.MarkBootstrapped(); err != nil {
		t.Fatalf("MarkBootstrapped: %v", err)
	}
	if !l.IsBootstrapped() {
		t.Fatal("IsBootstrapped() should be true after mark")
	}
	reloaded, err := LoadSignedLock(path, key)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Tampered() {
		t.Fatal("Tampered() should be false after round-trip")
	}
	if !reloaded.IsBootstrapped() {
		t.Fatal("IsBootstrapped() should survive reload")
	}
}

func TestLockMarkBootstrapped_Idempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFileName)
	l, _ := LoadSignedLock(path, testSigningKey())
	if err := l.MarkBootstrapped(); err != nil {
		t.Fatalf("first MarkBootstrapped: %v", err)
	}
	if err := l.MarkBootstrapped(); err != nil {
		t.Fatalf("second MarkBootstrapped (idempotent): %v", err)
	}
}

func TestLoadSignedLock_BootstrappedCoveredByMAC(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFileName)
	key := testSigningKey()
	l, _ := LoadSignedLock(path, key)
	_ = l.Record("repo/deploy", "abc", ApprovedByManual)
	_ = l.MarkBootstrapped()

	data, _ := os.ReadFile(path)
	// Flip bootstrapped: true to bootstrapped: false to test MAC coverage.
	flipped := strings.ReplaceAll(string(data), "bootstrapped: true", "bootstrapped: false")
	if flipped == string(data) {
		t.Fatal("bootstrapped: true not found in lock file after MarkBootstrapped; update the search string if YAML serialisation changed")
	}
	_ = os.WriteFile(path, []byte(flipped), 0600)

	reloaded, err := LoadSignedLock(path, key)
	if err != nil {
		t.Fatalf("LoadSignedLock after flag flip: %v", err)
	}
	if !reloaded.Tampered() {
		t.Fatal("flipping bootstrapped must invalidate MAC (Tampered() = true)")
	}
	if len(reloaded.List()) != 0 {
		t.Fatal("tampered lock must discard all records")
	}
}

func TestLoadSignedLock_V1UpgradeBootstrappedFalse(t *testing.T) {
	// A v1 unsigned lock (pre-v2/v3) should upgrade to v3 with bootstrapped=false.
	path := filepath.Join(t.TempDir(), LockFileName)
	key := testSigningKey()
	// Write unsigned v1 lock.
	unsigned, _ := LoadLock(path)
	_ = unsigned.Record("repo/deploy", "abc123", ApprovedByManual)

	// Load with signing key: upgrades to v3.
	upgraded, err := LoadSignedLock(path, key)
	if err != nil {
		t.Fatalf("LoadSignedLock on v1: %v", err)
	}
	if upgraded.Tampered() {
		t.Fatal("v1 upgrade must not be tampered")
	}
	if _, ok := upgraded.Get("repo/deploy"); !ok {
		t.Fatal("records must survive v1→v3 upgrade")
	}
	if upgraded.IsBootstrapped() {
		t.Fatal("bootstrapped must be false after upgrading from v1")
	}
	// Verify second load is clean v3.
	verified, err := LoadSignedLock(path, key)
	if err != nil || verified.Tampered() {
		t.Fatalf("v3 verify after upgrade: tampered=%v err=%v", verified.Tampered(), err)
	}
}

func TestLoadSignedLock_V2UpgradeBootstrappedFalse(t *testing.T) {
	// A genuine v2 lock (version=2, HMAC over tasks only) should upgrade to v3
	// with bootstrapped=false even if the file has bootstrapped: true in YAML,
	// because bootstrapped is not covered by the v2 MAC and must be ignored.
	path := filepath.Join(t.TempDir(), LockFileName)
	key := testSigningKey()

	// Build a v2 lock file manually: compute v2 MAC using the unexported helper.
	tasks := map[string]Record{
		"repo/deploy": {
			Hash:       "abc123",
			ApprovedBy: ApprovedByManual,
			ApprovedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	ephemeral := &Lock{path: path, signingKey: key, tasks: tasks}
	mac, err := ephemeral.computeMACv2()
	if err != nil {
		t.Fatalf("computeMACv2: %v", err)
	}
	// Write a v2 file with bootstrapped: true in the YAML — the loader must ignore it.
	lf := lockFile{Version: lockVersion, MAC: mac, Bootstrapped: true, Tasks: tasks}
	raw, err := yaml.Marshal(lf)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Load with signing key: should verify v2 MAC, ignore bootstrapped field, upgrade to v3.
	upgraded, err := LoadSignedLock(path, key)
	if err != nil {
		t.Fatalf("LoadSignedLock on v2: %v", err)
	}
	if upgraded.Tampered() {
		t.Fatal("v2 upgrade must not be tampered")
	}
	if _, ok := upgraded.Get("repo/deploy"); !ok {
		t.Fatal("records must survive v2→v3 upgrade")
	}
	if upgraded.IsBootstrapped() {
		t.Fatal("bootstrapped must be false after v2→v3 upgrade even if YAML field was true")
	}
	// Second load must be a valid v3 file.
	verified, err := LoadSignedLock(path, key)
	if err != nil || verified.Tampered() {
		t.Fatalf("v3 verify after v2 upgrade: tampered=%v err=%v", verified.Tampered(), err)
	}
}

func TestLoadSignedLock_RecordAfterTamperDetection(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockFileName)
	key := testSigningKey()

	l, _ := LoadSignedLock(path, key)
	_ = l.Record("repo/deploy", "abc", ApprovedByManual)

	// Tamper.
	data, _ := os.ReadFile(path)
	_ = os.WriteFile(path, []byte(strings.ReplaceAll(string(data), "abc", "bad")), 0600)

	tampered, _ := LoadSignedLock(path, key)
	if !tampered.Tampered() {
		t.Fatal("expected tampered state")
	}

	// Re-approval should succeed and produce a valid signed lock.
	if err := tampered.Record("repo/deploy", "abc", ApprovedByManual); err != nil {
		t.Fatalf("Record after tamper: %v", err)
	}

	reloaded, err := LoadSignedLock(path, key)
	if err != nil {
		t.Fatalf("reload after re-approval: %v", err)
	}
	if reloaded.Tampered() {
		t.Fatal("Tampered() should be false after re-approval")
	}
	rec, ok := reloaded.Get("repo/deploy")
	if !ok || rec.Hash != "abc" {
		t.Fatalf("record should survive re-approval: %+v ok=%v", rec, ok)
	}
}
