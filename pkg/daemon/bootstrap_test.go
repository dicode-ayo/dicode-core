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
