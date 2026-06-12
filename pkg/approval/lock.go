// Package approval implements the trust-on-change task approval gate (#392).
//
// Policy (what is trusted) is declared by the operator in dicode.yaml and
// never written by the daemon. Approval records (which content hash of which
// task is approved) live in dicode.lock, a daemon-owned sibling of
// dicode.yaml, package-lock style: human-readable, diffable, committable.
package approval

import (
	"fmt"
	"os"
	"path/filepath"
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
	ApprovedByGateDisabled  = "gate-disabled"
	ApprovedByBootstrap     = "bootstrap"
)

// Record is one approved task entry in the lock: the content hash that was
// approved, when, and through which path.
type Record struct {
	Hash       string    `yaml:"hash"`
	ApprovedAt time.Time `yaml:"approved_at"`
	ApprovedBy string    `yaml:"approved_by"`
}

// lockFile is the on-disk YAML shape of dicode.lock.
type lockFile struct {
	Version int               `yaml:"version"`
	Tasks   map[string]Record `yaml:"tasks"`
}

const lockVersion = 1

// Lock is the in-memory view of dicode.lock. All mutating methods persist
// to disk before returning. Safe for concurrent use.
type Lock struct {
	path string

	mu    sync.Mutex
	tasks map[string]Record
}

// LoadLock reads the lockfile at path, returning an empty Lock if the file
// does not exist yet.
func LoadLock(path string) (*Lock, error) {
	l := &Lock{path: path, tasks: map[string]Record{}}
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
	return l, nil
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
