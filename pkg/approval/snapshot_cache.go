// This file implements the sidecar persistence for the gate's
// last-known-approved content snapshot (#642). See Gate's approvedFiles doc
// comment in gate.go for the in-memory side this backs.
package approval

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"crypto/sha256"

	"github.com/dicode/dicode/internal/fsutil"
	"go.uber.org/zap"
)

// SnapshotCacheDirName is the sidecar cache directory name. The daemon
// creates it directly under data_dir (the same directory that holds the
// SQLite DB — see pkg/db and daemon.go's resolveDataDir) and passes that path
// to NewGate as cacheDir. Deliberately NOT dicode.lock or anywhere near it:
// the lock is documented as human-readable, diffable, and committable, and
// this cache holds full (redacted) file content, not just a hash — it must
// never end up in an operator's git history alongside the lock. data_dir, by
// contrast, is already daemon-private runtime state.
const SnapshotCacheDirName = "approval-snapshots"

// cachedSnapshot is the on-disk shape of one task's cached approved snapshot.
// Hash pins the exact content-hash generation this snapshot describes: the
// cache is keyed by task ID (via the file name, see cacheFileName) and
// validated against the *current* approval record in dicode.lock at every
// read, so a cache entry whose Hash no longer matches the lock's recorded
// hash for that task — the lock was updated by some other path, the task was
// forgotten and reborn, etc. — is treated as absent rather than stale data
// silently feeding a diff. This is what keeps the cache from ever needing its
// own invalidation logic: it can only ever agree with the lock or be ignored.
type cachedSnapshot struct {
	TaskID   string                   `json:"task_id"`
	Hash     string                   `json:"hash"`
	Files    map[string]snapshotValue `json:"files"`
	Resolved string                   `json:"resolved,omitempty"`
}

// snapshotCache reads and writes cachedSnapshot files under a directory. A
// nil *snapshotCache is a valid, inert receiver — every method is a no-op —
// so callers (Gate) never need a separate "is persistence enabled" check.
type snapshotCache struct {
	dir string
}

// newSnapshotCache builds a cache rooted at dir. An empty dir disables
// persistence entirely (returns nil): the daemon always has a data_dir, but
// tests and callers that don't care about restart survival can opt out by
// passing "".
func newSnapshotCache(dir string) *snapshotCache {
	if dir == "" {
		return nil
	}
	return &snapshotCache{dir: dir}
}

// cacheFileName derives a filesystem-safe file name from a task ID. Task IDs
// are namespaced with "/" (e.g. "repo/deploy") and are otherwise
// operator/source-controlled text, neither of which is safe to use directly
// as a path component (traversal, nesting, length/character limits) — hashing
// sidesteps all of that without needing to validate or escape the ID.
func cacheFileName(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:]) + ".json"
}

// path returns the on-disk location of id's cache entry.
func (c *snapshotCache) path(id string) string {
	return filepath.Join(c.dir, cacheFileName(id))
}

// save persists id's approved snapshot, tagged with hash (the hash dicode.lock
// currently records as approved for id). A no-op when there is nothing worth
// persisting (dir-less task, or no hash to key it by) or when the cache is
// disabled (nil receiver).
//
// No additional size/count bounds are applied here: files is exactly the map
// snapshotDir already produced under maxSnapshotFileBytes / maxSnapshotFiles
// (pkg/approval/snapshot.go) — this only ever serializes what the gate has
// already capped, never a second, independent limit.
func (c *snapshotCache) save(id, hash string, files map[string]snapshotValue, resolved string) error {
	if c == nil || hash == "" || files == nil {
		return nil
	}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return fmt.Errorf("approval: create snapshot cache dir %s: %w", c.dir, err)
	}
	data, err := json.Marshal(cachedSnapshot{TaskID: id, Hash: hash, Files: files, Resolved: resolved})
	if err != nil {
		return fmt.Errorf("approval: marshal snapshot cache for %q: %w", id, err)
	}
	// 0600: this cache holds the same redacted-but-still-sensitive task
	// content the in-memory approvedFiles map does — same mode dicode.lock
	// itself is written with.
	if err := fsutil.WriteFileAtomic(c.path(id), data, 0o600); err != nil {
		return fmt.Errorf("approval: write snapshot cache for %q: %w", id, err)
	}
	return nil
}

// load returns id's cached snapshot, but only when it is tagged with exactly
// wantHash — the hash dicode.lock currently records as approved for id. Any
// mismatch (different hash, corrupt/missing file, unreadable JSON) is treated
// as "nothing cached" rather than an error: this is a best-effort restart
// optimization, never a source of truth, and the gate's existing behavior
// (rebuild the baseline on the next real approval) is a perfectly safe
// fallback for every failure mode here.
func (c *snapshotCache) load(id, wantHash string) (files map[string]snapshotValue, resolved string, ok bool) {
	if c == nil || wantHash == "" {
		return nil, "", false
	}
	data, err := os.ReadFile(c.path(id))
	if err != nil {
		return nil, "", false
	}
	var cs cachedSnapshot
	if err := json.Unmarshal(data, &cs); err != nil {
		return nil, "", false
	}
	if cs.TaskID != id || cs.Hash != wantHash || cs.Files == nil {
		return nil, "", false
	}
	return cs.Files, cs.Resolved, true
}

// delete removes id's cache entry, if any. Called from Gate.Forget so a
// forgotten-then-reborn task ID cannot later load a snapshot cache entry that
// has nothing to do with its new incarnation. Not otherwise required for
// correctness — load already refuses anything whose Hash doesn't match the
// current lock record — but leaving orphaned entries around after Forget
// would let the cache directory grow unbounded across the lifetime of a
// daemon that repeatedly adds and removes tasks.
func (c *snapshotCache) delete(id string) {
	if c == nil {
		return
	}
	_ = os.Remove(c.path(id))
}

// persistApprovedSnapshot writes id's current in-memory approved snapshot
// (g.approvedFiles / g.approvedResolved) to the on-disk cache, tagged with
// the hash dicode.lock currently records as approved for id. Called from
// every Admit/approve path that just finished treating the current content
// as approved (see call sites in gate.go). A missing lock record, an empty
// hash, or a dir-less task (approvedFiles[id] == nil) are silent no-ops —
// mirrors snapshotCache.save's own tolerance, since there is nothing wrong
// here, just nothing worth caching.
//
// Read failures are logged, not propagated: like every other snapshot
// operation in this package (see takeSnapshot), a diff-support write must
// never block or fail the arm/approve decision it is piggybacking on.
func (g *Gate) persistApprovedSnapshot(id string) {
	if g.snapCache == nil {
		return
	}
	rec, ok := g.lock.Get(id)
	if !ok || rec.Hash == "" {
		return
	}
	g.mu.Lock()
	files := g.approvedFiles[id]
	resolved := g.approvedResolved[id]
	g.mu.Unlock()
	if files == nil {
		return
	}
	if err := g.snapCache.save(id, rec.Hash, files, resolved); err != nil {
		g.log.Warn("approval: persist approved snapshot failed", zap.String("task", id), zap.Error(err))
	}
}

// loadCachedApprovedIfMissing populates g.approvedFiles[id] (and
// g.approvedResolved[id]) from the on-disk cache when nothing is cached for
// id yet in this gate's lifetime — the daemon-restart case #642 exists to
// fix: a task pending at a hash that differs from its last-approved one has
// no in-memory baseline until the next successful approval, so Gate.Diff
// reports HasBaseline=false even though a perfectly good "before" snapshot
// was captured and persisted the last time this task was approved, possibly
// in a previous process.
//
// Deliberately mirrors snapshotApprovedIfMissing's "only when missing" guard:
// once populated (by this, or by any of the directory-walk paths), later
// Admit calls in the same process skip straight past this — an in-memory hit
// is always at least as fresh as the on-disk cache.
func (g *Gate) loadCachedApprovedIfMissing(id string) {
	if g.snapCache == nil {
		return
	}
	g.mu.Lock()
	_, exists := g.approvedFiles[id]
	g.mu.Unlock()
	if exists {
		return
	}
	rec, ok := g.lock.Get(id)
	if !ok || rec.Hash == "" {
		return
	}
	files, resolved, ok := g.snapCache.load(id, rec.Hash)
	if !ok {
		return
	}
	g.mu.Lock()
	g.approvedFiles[id] = files
	if resolved != "" {
		g.approvedResolved[id] = resolved
	}
	g.mu.Unlock()
}
