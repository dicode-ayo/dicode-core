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
	"path/filepath"
	"strings"
	"sync"
	"time"

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
	MAC   string            `yaml:"mac,omitempty"`
	Tasks map[string]Record `yaml:"tasks"`
}

const (
	lockVersionUnsigned = 1 // legacy; no MAC field
	lockVersion         = 2 // HMAC-signed
)

// Lock is the in-memory view of dicode.lock. All mutating methods persist
// to disk before returning. Safe for concurrent use.
type Lock struct {
	path       string
	signingKey []byte // nil = unsigned (legacy / test) mode
	tampered   bool   // true when MAC verification failed; all records discarded

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

	if key == nil {
		// Unsigned mode: accept without verification.
		return l, nil
	}

	if lf.MAC == "" {
		// Legacy unsigned lock: seal immediately so future loads verify.
		if err := l.save(); err != nil {
			return nil, fmt.Errorf("seal legacy %s: %w", path, err)
		}
		return l, nil
	}

	if !l.verifyMAC(lf.MAC) {
		// Tampered or forged lock: discard all records and fail closed.
		// The caller learns of this via Tampered() and should warn the operator.
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

// macContent returns the canonical JSON that is HMAC'd. Go's encoding/json
// sorts map keys, making this deterministic for the same set of records.
func (l *Lock) macContent() ([]byte, error) {
	return json.Marshal(l.tasks)
}

// computeMAC returns the HMAC-SHA256 hex digest over the canonical JSON of
// the tasks map. Caller must hold mu (or call before mu is needed).
func (l *Lock) computeMAC() (string, error) {
	data, err := l.macContent()
	if err != nil {
		return "", fmt.Errorf("compute MAC: %w", err)
	}
	mac := hmac.New(sha256.New, l.signingKey)
	mac.Write(data)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// verifyMAC reports whether macHex matches the HMAC of the current tasks map.
// The stored hex is normalized to lowercase before comparison so that a file
// manually edited with uppercase hex doesn't trigger a false tamper alarm.
func (l *Lock) verifyMAC(macHex string) bool {
	expected, err := l.computeMAC()
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

// save persists the lock atomically (temp file + rename). Caller must hold mu.
func (l *Lock) save() error {
	lf := lockFile{Version: lockVersion, Tasks: l.tasks}
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
	tmp, err := os.CreateTemp(filepath.Dir(l.path), ".dicode.lock.*")
	if err != nil {
		return fmt.Errorf("write %s: %w", l.path, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(header, data...)); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write %s: %w", l.path, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("write %s: %w", l.path, err)
	}
	if err := os.Rename(tmpName, l.path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("write %s: %w", l.path, err)
	}
	return nil
}
