package daemon

import (
	"context"
	"testing"

	"github.com/dicode/dicode/pkg/db"
)

// TestShouldBootstrap covers the fail-closed decision matrix. The key case is
// (marker present, lock absent): the approval lock vanished after a prior run,
// which must NOT re-enter bootstrap or the inventory's pending changes would be
// re-seeded as approved (the #402 escalation).
func TestShouldBootstrap(t *testing.T) {
	cases := []struct {
		name          string
		lockExisted   bool
		markerExists  bool
		policyEnabled bool
		want          bool
	}{
		{"genuine first run", false, false, true, true},
		{"lock lost after prior run fails closed", false, true, true, false},
		{"normal start with lock present", true, true, true, false},
		{"lock present without marker", true, false, true, false},
		{"gate disabled never bootstraps", false, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldBootstrap(tc.lockExisted, tc.markerExists, tc.policyEnabled); got != tc.want {
				t.Errorf("shouldBootstrap(%v, %v, %v) = %v, want %v",
					tc.lockExisted, tc.markerExists, tc.policyEnabled, got, tc.want)
			}
		})
	}
}

// TestBootstrapMarkerRoundTrip verifies the marker is absent on a fresh DB,
// present after setBootstrapMarker, and that setting it twice is idempotent.
func TestBootstrapMarkerRoundTrip(t *testing.T) {
	database, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()
	ctx := context.Background()

	exists, err := bootstrapMarkerExists(ctx, database)
	if err != nil {
		t.Fatalf("marker check on fresh db: %v", err)
	}
	if exists {
		t.Fatal("marker present on a fresh db; expected absent (genuine first run)")
	}

	if err := setBootstrapMarker(ctx, database); err != nil {
		t.Fatalf("set marker: %v", err)
	}
	if err := setBootstrapMarker(ctx, database); err != nil {
		t.Fatalf("set marker again (idempotent): %v", err)
	}

	exists, err = bootstrapMarkerExists(ctx, database)
	if err != nil {
		t.Fatalf("marker check after set: %v", err)
	}
	if !exists {
		t.Fatal("marker absent after setBootstrapMarker; expected present")
	}
}

// TestShouldBootstrap_AdoptedLockBackfillFailsClosed documents the fail-closed
// invariant end-to-end: an adopted lock (present at startup, e.g. operator-
// shipped or written before a crash interrupted the first bootstrap) backfills
// the marker, so a later lock-loss is correctly held pending instead of being
// re-seeded as approved (the #402 escalation). Without the backfill, the
// post-deletion state would be (lock absent, marker absent) → bootstrap re-runs.
func TestShouldBootstrap_AdoptedLockBackfillFailsClosed(t *testing.T) {
	// Startup with a lock present and no marker yet: the gate must not bootstrap.
	if shouldBootstrap(true, false, true) {
		t.Fatal("adopting an existing lock must not enter bootstrap")
	}
	// The adopt path backfills the marker; a subsequent lock-loss then presents
	// (lock absent, marker present), which must fail closed.
	if shouldBootstrap(false, true, true) {
		t.Fatal("lock lost after marker backfill must fail closed, not re-seed as approved")
	}
}

// TestBootstrapMarkerKeyIsTaskUnforgeable guards the invariant that the marker
// key carries no colon. Task kv rows are namespaced "<taskID>:<key>" by the IPC
// server, so any task-reachable row contains a colon; a colon-free key can
// never be forged or deleted by a task.
func TestBootstrapMarkerKeyIsTaskUnforgeable(t *testing.T) {
	for i := 0; i < len(approvalBootstrapMarkerKey); i++ {
		if approvalBootstrapMarkerKey[i] == ':' {
			t.Fatalf("marker key %q contains ':' — a task could forge it via its kv namespace",
				approvalBootstrapMarkerKey)
		}
	}
}

// TestBootstrapMarkerDBDeleteFallback verifies that the DB-deletion attack vector
// is closed: if the DB (and thus its bootstrap marker) is wiped but the lock's
// bootstrapped flag is still true, shouldBootstrap must remain false.
func TestBootstrapMarkerDBDeleteFallback(t *testing.T) {
	// Simulate: DB deleted (dbMarkerExists=false) but lock.IsBootstrapped()=true.
	lockBootstrapped := true
	dbMarkerExists := false
	markerExists := lockBootstrapped || dbMarkerExists
	// lockExisted=true: the v3 lock file still exists (only the DB was wiped).
	if shouldBootstrap(true, markerExists, true) {
		t.Fatal("DB deletion must not re-enable bootstrap when lock has bootstrapped=true")
	}
}

// TestBootstrapMarkerLockDeleteFallback verifies that lock-loss alone
// does not re-enable bootstrap when the DB marker is still present.
func TestBootstrapMarkerLockDeleteFallback(t *testing.T) {
	// Simulate: lock deleted (bootstrapped=false) but DB marker exists.
	lockBootstrapped := false
	dbMarkerExists := true
	markerExists := lockBootstrapped || dbMarkerExists
	// lock absent + marker present → shouldBootstrap=false (fail closed)
	if shouldBootstrap(false, markerExists, true) {
		t.Fatal("lock-deletion alone must not re-enable bootstrap when DB marker exists")
	}
}
