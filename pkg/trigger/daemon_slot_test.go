package trigger

import (
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/registry"
)

// TestDaemonSlot_StateMachine drives the slot through every transition and
// checks the invariant (a second claim fails iff the slot is occupied) and the
// run the slot ends up bound to.
func TestDaemonSlot_StateMachine(t *testing.T) {
	type step struct {
		op      string
		arg     string
		wantOK  bool   // result of ops that return bool/err-nil
		wantRun string // slot-bound run after the step
		// wantClaimable: whether a fresh claim of the same ID would succeed.
		wantClaimable bool
	}
	cases := []struct {
		name  string
		steps []step
	}{
		{"claim attach release", []step{
			{"claim", "", true, "", false},
			{"attach", "r1", true, "r1", false},
			{"release", "r1", true, "", true},
		}},
		{"double claim coalesces", []step{
			{"claim", "", true, "", false},
			{"claim", "", false, "", false},
		}},
		{"release of superseded run is a no-op", []step{
			{"claim", "", true, "", false},
			{"attach", "r1", true, "r1", false},
			{"adopt", "r2", true, "r2", false},
			{"release", "r1", false, "r2", false},
			{"release", "r2", true, "", true},
		}},
		{"park then adopt then release", []step{
			{"claim", "", true, "", false},
			{"attach", "r1", true, "r1", false},
			{"park", "", true, "r1", false},
			{"adopt", "r2", true, "r2", false},
			{"release", "r2", true, "", true},
		}},
		{"unregister evicts and inerts the handle", []step{
			{"claim", "", true, "", false},
			{"attach", "r1", true, "r1", false},
			{"unregister", "", true, "", true},
			{"attach", "r2", false, "", true},
			{"release", "r1", false, "", true},
			{"park", "", false, "", true},
			{"adopt", "r3", false, "", true},
		}},
		{"stale handle cannot clobber a newer claim", []step{
			{"claim", "", true, "", false},
			{"attach", "r1", true, "r1", false},
			{"unregister", "", true, "", true},
			{"claim2", "", true, "", false},
			{"attach2", "r9", true, "r9", false},
			{"release", "r1", false, "r9", false},
			{"attach", "r2", false, "r9", false},
			{"adopt", "r3", false, "r9", false},
		}},
		{"sweep releases only the swept run", []step{
			{"claim", "", true, "", false},
			{"attach", "r1", true, "r1", false},
			{"sweep", "r0", false, "r1", false},
			{"sweep", "r1", true, "", true},
		}},
		{"sweep after adoption leaves continuation", []step{
			{"claim", "", true, "", false},
			{"attach", "r1", true, "r1", false},
			{"park", "", true, "r1", false},
			{"adopt", "r2", true, "r2", false},
			{"sweep", "r1", false, "r2", false},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newDaemonSlots()
			const id = "d"
			var h, h2 daemonSlot
			for i, st := range tc.steps {
				var got bool
				switch st.op {
				case "claim":
					h, got = s.claim(id)
				case "claim2":
					h2, got = s.claim(id)
				case "attach":
					got = h.attach(st.arg)
				case "attach2":
					got = h2.attach(st.arg)
				case "park":
					cur := h
					cur.run = s.runID(id)
					got = cur.park()
				case "adopt":
					cur := h
					cur.run = s.runID(id)
					got = cur.adopt(st.arg) == nil
				case "release":
					got = h.release(st.arg)
				case "unregister":
					s.evict(id)
					got = true
				case "sweep":
					got = s.releaseRun(id, st.arg)
				}
				if got != st.wantOK {
					t.Fatalf("step %d %s(%q) = %v, want %v", i, st.op, st.arg, got, st.wantOK)
				}
				if r := s.runID(id); r != st.wantRun {
					t.Fatalf("step %d %s: bound run = %q, want %q", i, st.op, r, st.wantRun)
				}
				probe, ok := s.claim(id)
				if ok != st.wantClaimable {
					t.Fatalf("step %d %s: claimable = %v, want %v", i, st.op, ok, st.wantClaimable)
				}
				if ok {
					probe.release("")
				}
			}
		})
	}
}

func TestDaemonSlot_PipelineKeyspaceIsSeparate(t *testing.T) {
	s := newDaemonSlots()
	d, ok := s.claim("x")
	if !ok {
		t.Fatal("daemon claim failed")
	}
	p, ok := s.claimPipeline("x")
	if !ok {
		t.Fatal("pipeline claim with the same ID must not coalesce with the daemon slot")
	}
	if _, ok := s.claimPipeline("x"); ok {
		t.Fatal("second pipeline claim must coalesce")
	}
	p.release("")
	d.release("")
}

// TestUnregister_EvictsInFlightStartClaim: a start that raced Unregister never
// launches a body or publishes DaemonRunning.
func TestUnregister_EvictsInFlightStartClaim(t *testing.T) {
	eng, spec, exec := daemonRaceEnv(t, "never")

	slot, ok := eng.slots.claim(spec.ID)
	if !ok {
		t.Fatal("claim failed")
	}
	eng.Unregister(spec.ID)

	if _, ok := eng.slots.claim(spec.ID); !ok {
		t.Fatal("Unregister must free the in-flight start's claim")
	}
	eng.slots.evict(spec.ID)

	eng.launchDaemon(spec, slot)
	time.Sleep(100 * time.Millisecond)
	if got := exec.runs.Load(); got != 0 {
		t.Fatalf("executed %d bodies for a start that lost the race to Unregister, want 0", got)
	}
	if got := eng.DaemonState(spec.ID); got != DaemonStopped {
		t.Fatalf("DaemonState = %q, want stopped", got)
	}
}

// A superseding claim made after Unregister must survive a finish handler of
// the evicted body.
func TestUnregister_DoesNotClobberSupersedingClaim(t *testing.T) {
	eng, spec, _ := daemonRaceEnv(t, "never")
	runID := quickFailureRun(t, eng, spec.ID, registry.StatusFailure)
	seedSlot(eng, spec.ID, runID)
	eng.Unregister(spec.ID)
	seedSlot(eng, spec.ID, "fresh-run")
	eng.onDaemonRunFinished(spec, runID)
	if got := eng.slots.runID(spec.ID); got != "fresh-run" {
		t.Fatalf("slot = %q, want superseding run left untouched", got)
	}
}

// TestDaemonSlots_EmptyRunIDNeverMatchesFreshClaim: a fresh claim has runID "",
// which holding/releaseRun/adopt must not treat as a match.
func TestDaemonSlots_EmptyRunIDNeverMatchesFreshClaim(t *testing.T) {
	s := newDaemonSlots()
	h, ok := s.claim("t")
	if !ok {
		t.Fatal("claim failed")
	}
	if _, ok := s.holding("t", ""); ok {
		t.Fatal("holding(\"\") matched a fresh claim")
	}
	if s.releaseRun("t", "") {
		t.Fatal("releaseRun(\"\") freed a fresh claim")
	}
	if _, ok := s.claim("t"); ok {
		t.Fatal("fresh claim was released by an empty run ID")
	}
	if err := h.adopt("r1"); err == nil {
		t.Fatal("adopt from an unbound claim succeeded")
	}
	h.attach("r1")
	hold, ok := s.holding("t", "r1")
	if !ok {
		t.Fatal("holding(r1) failed")
	}
	if err := hold.adopt(""); err == nil {
		t.Fatal("adopt(\"\") succeeded")
	}
	if got := s.runID("t"); got != "r1" {
		t.Fatalf("slot run = %q, want r1", got)
	}
}

// TestDaemonSlot_ZeroValueHandleIsInert: the handle returned by a failed claim
// or holding must be safe to use.
func TestDaemonSlot_ZeroValueHandleIsInert(t *testing.T) {
	var h daemonSlot
	if h.attach("r") {
		t.Error("attach on zero handle succeeded")
	}
	if h.park() {
		t.Error("park on zero handle succeeded")
	}
	if err := h.adopt("r"); err == nil {
		t.Error("adopt on zero handle succeeded")
	}
	if h.release("r") || h.release("") {
		t.Error("release on zero handle succeeded")
	}
	if h.bound("r") {
		t.Error("bound on zero handle is true")
	}
}
