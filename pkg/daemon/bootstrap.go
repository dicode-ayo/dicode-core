package daemon

import (
	"context"
	"path/filepath"

	"github.com/dicode/dicode/pkg/db"
)

// canonicalPath resolves symlinks in p so a protected path compares equal to
// the form a task's write resolves to (e.g. a config dir reached via a symlink
// such as macOS /var → /private/var). Falls back to the cleaned path when the
// target does not yet exist or cannot be resolved.
func canonicalPath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

// approvalBootstrapMarkerKey records that the approval gate has completed its
// first-run seeding at least once. It lives in the kv table — and there every
// task key is namespaced as "<taskID>:<key>", so this colon-free key is
// unforgeable/undeleteable via the kv IPC API.
//
// DB-deletion attack: a task with broad fs.write can delete the SQLite DB
// file, erasing this marker. To close that vector, the same flag is also
// embedded in dicode.lock (covered by its HMAC). At daemon startup the
// effective marker is the OR of this DB row and lock.IsBootstrapped(), so
// deleting either alone is insufficient to re-enable bootstrap.
const approvalBootstrapMarkerKey = "approval.bootstrap_completed"

// bootstrapMarkerExists reports whether the first-run seeding marker is set.
func bootstrapMarkerExists(ctx context.Context, database db.DB) (bool, error) {
	var found bool
	err := database.Query(ctx,
		`SELECT 1 FROM kv WHERE key = ?`, []any{approvalBootstrapMarkerKey},
		func(rows db.Scanner) error {
			if rows.Next() {
				found = true
			}
			return nil
		},
	)
	return found, err
}

// setBootstrapMarker records that first-run seeding has completed. Idempotent.
func setBootstrapMarker(ctx context.Context, database db.DB) error {
	return database.Exec(ctx,
		`INSERT INTO kv (key, value) VALUES (?, '1')
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		approvalBootstrapMarkerKey,
	)
}

// shouldBootstrap decides whether to seed the current inventory as approved.
//
// markerExists is the OR of the lock's bootstrapped flag (lock.IsBootstrapped)
// and the DB kv marker (bootstrapMarkerExists). Both must be absent to permit
// re-bootstrap, so deleting either alone (the SQLite DB or the lock file) cannot
// reset the gate.
//
// Security invariant: an approval lock that disappears after the daemon has run
// before must fail closed. A task with a broad fs-write grant can delete
// dicode.lock by a vector the file-level deny does not cover (removing the
// containing dir, renaming over it). Re-entering bootstrap on the next start
// would then re-seed that task's pending change as approved — the #402
// escalation. The persisted marker distinguishes a genuine first run (marker
// absent) from a lock that vanished after a prior run (marker present), so
// bootstrap is entered only when both the lock and the marker are absent.
func shouldBootstrap(lockExisted, markerExists, policyEnabled bool) bool {
	return policyEnabled && !lockExisted && !markerExists
}
