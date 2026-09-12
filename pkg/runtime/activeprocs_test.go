package runtime

import (
	"slices"
	"testing"
)

// TestTrackProcess_RegistersAndReleases: the tracker is what metrics
// aggregate child resource usage over and what the daemon's drain counts, so
// a PID has to appear for exactly as long as its process is running.
func TestTrackProcess_RegistersAndReleases(t *testing.T) {
	const pid = 987654321
	if slices.Contains(ActivePIDs(), pid) {
		t.Fatalf("pid %d tracked before the test started", pid)
	}

	release := TrackProcess(pid)
	if !slices.Contains(ActivePIDs(), pid) {
		t.Errorf("ActivePIDs() = %v; want it to contain %d", ActivePIDs(), pid)
	}

	release()
	if slices.Contains(ActivePIDs(), pid) {
		t.Errorf("ActivePIDs() = %v; want %d gone after release", ActivePIDs(), pid)
	}
}

// TestTrackProcess_ReleaseIsIdempotent: both runtimes release on every exit
// path of a retry loop, so a double release must not drop a re-registered PID.
func TestTrackProcess_ReleaseIsIdempotent(t *testing.T) {
	const pid = 987654322
	release := TrackProcess(pid)
	release()
	release()
	if slices.Contains(ActivePIDs(), pid) {
		t.Errorf("ActivePIDs() = %v; want %d gone", ActivePIDs(), pid)
	}
}
