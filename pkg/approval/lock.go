// Package approval implements the trust-on-change task approval gate (#392).
//
// Policy (what is trusted) is declared by the operator in dicode.yaml and
// never written by the daemon. Approval records (which content hash of which
// task is approved) live in dicode.lock, a daemon-owned sibling of
// dicode.yaml, package-lock style: human-readable, diffable, committable.
package approval

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/dicode/dicode/internal/fsutil"
	"gopkg.in/yaml.v3"
)

// LockFileName is the name of the daemon-owned approval lockfile, written
// next to dicode.yaml.
const LockFileName = "dicode.lock"

// ApprovedBy values recorded in the lock. Open set — later phases may add
// more (e.g. token-link approvals).
const (
	ApprovedByBuiltin       = "builtin"
	ApprovedByTrustedSource = "trusted-source"
	ApprovedByTrustedTask   = "trusted-task"
	ApprovedByManual        = "manual"
	ApprovedByToken         = "token-link"
	ApprovedByGateDisabled  = "gate-disabled"
	ApprovedByBootstrap     = "bootstrap"
)

// Record is one approved task entry in the lock: the content hash that was
// approved, when, and through which path.
type Record struct {
	Hash       string    `yaml:"hash"        json:"hash"`
	ApprovedAt time.Time `yaml:"approved_at" json:"approved_at"`
	ApprovedBy string    `yaml:"approved_by" json:"approved_by"`
}

// lockFile is the on-disk YAML shape of dicode.lock.
type lockFile struct {
	Version int `yaml:"version"`
	// MAC is the HMAC-SHA256 (hex) of the canonical JSON of the tasks map,
	// keyed by the daemon's lock-signing sub-key. Absent on unsigned v1 files.
	MAC          string            `yaml:"mac,omitempty"`
	Bootstrapped bool              `yaml:"bootstrapped,omitempty"`
	Tasks        map[string]Record `yaml:"tasks"`
}

const (
	lockVersionUnsigned     = 1 // legacy; no MAC field
	lockVersion             = 2 // HMAC over tasks only
	lockVersionBootstrapped = 3 // HMAC over {bootstrapped, tasks}
)

// Lock is the in-memory view of dicode.lock. All mutating methods persist
// to disk before returning. Safe for concurrent use.
type Lock struct {
	path         string
	signingKey   []byte // nil = unsigned (legacy / test) mode
	tampered     bool   // true when MAC verification failed; all records discarded
	bootstrapped bool   // true after MarkBootstrapped is called

	mu    sync.Mutex
	tasks map[string]Record
}

// LoadLock reads the lockfile without MAC verification (unsigned mode).
// Use LoadSignedLock when a signing key is available.
func LoadLock(path string) (*Lock, error) {
	return LoadSignedLock(path, nil)
}

// LoadSignedLock reads and integrity-checks the lockfile.
//
// When key is nil the lock is loaded without verification (unsigned mode,
// preserved for tests and callers without access to the master key).
//
// When key is non-nil:
//   - Legacy v1 file (no mac field): accepted and immediately re-signed on
//     disk so subsequent loads verify correctly (format upgrade).
//   - MAC present and valid: accepted normally.
//   - MAC present but invalid: lock is treated as tampered — all records are
//     discarded (fail closed) and Tampered() returns true. The daemon should
//     log a warning and require explicit re-approval of all tasks.
func LoadSignedLock(path string, key []byte) (*Lock, error) {
	l := &Lock{path: path, signingKey: key, tasks: map[string]Record{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return l, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var lf lockFile
	if err := yaml.Unmarshal(data, &lf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if lf.Tasks != nil {
		l.tasks = lf.Tasks
	}
	// l.bootstrapped is intentionally NOT set here. Only the v3 authenticated
	// path below may populate it; unsigned and v2 files do not cover this field
	// in their MAC so an attacker who can write to the lock cannot forge the flag.

	if key == nil {
		// Unsigned mode: accept without verification.
		return l, nil
	}

	if lf.MAC == "" {
		// MAC absent: only treat as a legacy v1 lock (version ≤ 1). A v2/v3
		// file with the mac field stripped is treated as tampered so an attacker
		// who can write to the lock file cannot remove the MAC to bypass
		// bootstrapped-flag protection or version gating.
		if lf.Version > lockVersionUnsigned {
			l.tampered = true
			l.tasks = map[string]Record{}
			return l, nil
		}
		// Legacy unsigned lock (v1): seal immediately so future loads verify.
		if err := l.save(); err != nil {
			return nil, fmt.Errorf("seal legacy %s: %w", path, err)
		}
		return l, nil
	}

	switch lf.Version {
	case lockVersion: // v2: HMAC over tasks only
		if !l.verifyMACv2(lf.MAC) {
			// Tampered or forged lock: discard all records and fail closed.
			l.tampered = true
			l.tasks = map[string]Record{}
			return l, nil
		}
		// Valid v2 — bootstrapped is not covered by the v2 MAC so it is forced
		// to false regardless of what the file field says.
		l.bootstrapped = false
		// Upgrade to v3 in-place so subsequent loads use the new MAC.
		if err := l.save(); err != nil {
			return nil, fmt.Errorf("upgrade %s to v3: %w", path, err)
		}
	case lockVersionBootstrapped: // v3: HMAC over {bootstrapped, tasks}
		// Set bootstrapped before MAC verification so macContent() uses the
		// correct value; cleared below if the MAC does not verify.
		l.bootstrapped = lf.Bootstrapped
		if !l.verifyMAC(lf.MAC) {
			// Tampered or forged lock: discard all records and fail closed.
			l.tampered = true
			l.bootstrapped = false
			l.tasks = map[string]Record{}
			return l, nil
		}
	default:
		// Unknown version with a MAC present — we cannot verify it; fail closed.
		l.tampered = true
		l.tasks = map[string]Record{}
		return l, nil
	}
	return l, nil
}

// Tampered reports whether the lock was loaded with a MAC that failed
// verification. When true all approval records have been discarded; every
// task requires explicit re-approval.
func (l *Lock) Tampered() bool { return l.tampered }

// macPayloadV3 is the MAC input for v3 locks — covers both bootstrapped and tasks.
type macPayloadV3 struct {
	Bootstrapped bool              `json:"bootstrapped"`
	Tasks        map[string]Record `json:"tasks"`
}

// macContent returns the canonical JSON HMAC'd for the current (v3) format.
func (l *Lock) macContent() ([]byte, error) {
	return json.Marshal(macPayloadV3{Bootstrapped: l.bootstrapped, Tasks: l.tasks})
}

// macContentV2 returns the canonical JSON HMAC'd for legacy v2 format (tasks only).
func (l *Lock) macContentV2() ([]byte, error) {
	return json.Marshal(l.tasks)
}

// computeMAC returns the HMAC-SHA256 hex digest over the canonical JSON of
// the v3 payload. Caller must hold mu (or call before mu is needed).
func (l *Lock) computeMAC() (string, error) {
	data, err := l.macContent()
	if err != nil {
		return "", fmt.Errorf("compute MAC: %w", err)
	}
	mac := hmac.New(sha256.New, l.signingKey)
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// computeMACv2 returns the HMAC-SHA256 hex digest over the canonical JSON of
// the v2 (tasks-only) payload. Used during v2→v3 upgrade verification.
func (l *Lock) computeMACv2() (string, error) {
	data, err := l.macContentV2()
	if err != nil {
		return "", fmt.Errorf("compute MAC v2: %w", err)
	}
	mac := hmac.New(sha256.New, l.signingKey)
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// verifyMAC reports whether macHex matches the v3 HMAC of the current lock.
// The stored hex is normalized to lowercase before comparison so that a file
// manually edited with uppercase hex doesn't trigger a false tamper alarm.
func (l *Lock) verifyMAC(macHex string) bool {
	expected, err := l.computeMAC()
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(expected), []byte(strings.ToLower(macHex)))
}

// verifyMACv2 reports whether macHex matches the v2 (tasks-only) HMAC.
func (l *Lock) verifyMACv2(macHex string) bool {
	expected, err := l.computeMACv2()
	if err != nil {
		return false
	}
	return hmac.Equal([]byte(expected), []byte(strings.ToLower(macHex)))
}

// Path returns the lockfile location on disk.
func (l *Lock) Path() string { return l.path }

// Approved reports whether the recorded hash for id matches hash. An empty
// hash never matches — a task whose hash could not be computed cannot be
// considered approved.
func (l *Lock) Approved(id, hash string) bool {
	if hash == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.tasks[id]
	return ok && rec.Hash == hash
}

// Get returns the record for id, if any.
func (l *Lock) Get(id string) (Record, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.tasks[id]
	return rec, ok
}

// Record writes (or updates) the approved hash for id and persists the lock.
// A no-op when the recorded hash already matches, so steady-state reconciles
// don't rewrite the file or reset approved_at.
func (l *Lock) Record(id, hash, by string) error {
	if hash == "" {
		return fmt.Errorf("approval lock: refusing to record empty hash for %q", id)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if rec, ok := l.tasks[id]; ok && rec.Hash == hash {
		return nil
	}
	l.tasks[id] = Record{Hash: hash, ApprovedAt: time.Now().UTC(), ApprovedBy: by}
	return l.save()
}

// Remove drops the record for id and persists the lock. A no-op when the
// id is not present.
func (l *Lock) Remove(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.tasks[id]; !ok {
		return nil
	}
	delete(l.tasks, id)
	return l.save()
}

// List returns a copy of all records keyed by task ID.
func (l *Lock) List() map[string]Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]Record, len(l.tasks))
	for k, v := range l.tasks {
		out[k] = v
	}
	return out
}

// IsBootstrapped reports whether the first-run bootstrap has completed,
// as recorded in the lock file (independent of the SQLite kv table).
func (l *Lock) IsBootstrapped() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.bootstrapped
}

// MarkBootstrapped records that the first-run bootstrap has completed by
// setting the bootstrapped flag in the lock file. Idempotent.
func (l *Lock) MarkBootstrapped() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.bootstrapped {
		return nil
	}
	l.bootstrapped = true
	if err := l.save(); err != nil {
		// Roll back in-memory flag so the next call can retry the disk write.
		l.bootstrapped = false
		return err
	}
	return nil
}

// save persists the lock atomically (temp file + rename). Caller must hold mu.
func (l *Lock) save() error {
	lf := lockFile{
		Version:      lockVersionBootstrapped,
		Bootstrapped: l.bootstrapped,
		Tasks:        l.tasks,
	}
	if l.signingKey != nil {
		mac, err := l.computeMAC()
		if err != nil {
			return fmt.Errorf("sign %s: %w", l.path, err)
		}
		lf.MAC = mac
	} else {
		lf.Version = lockVersionUnsigned
	}
	data, err := yaml.Marshal(lf)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", l.path, err)
	}
	header := []byte("# dicode.lock — approval records, managed by the dicode daemon.\n" +
		"# Trust policy lives in dicode.yaml (approval:); this file records which\n" +
		"# content hash of each task is approved to run.\n")
	// 0600 matches the mode the previous temp-file dance produced.
	if err := fsutil.WriteFileAtomic(l.path, append(header, data...), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", l.path, err)
	}
	return nil
}
