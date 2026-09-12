package runtime

import "sync"

// activePIDs holds the PIDs of every live task subprocess, across runtimes.
var activePIDs sync.Map // map[int]struct{}

// TrackProcess registers a running task subprocess and returns the function
// that unregisters it. Both socket-bridge runtimes track their children here:
// the set feeds per-child resource metrics and the daemon's shutdown sweep,
// and a runtime that skips it makes its subprocesses invisible to both.
func TrackProcess(pid int) func() {
	activePIDs.Store(pid, struct{}{})
	return func() { activePIDs.Delete(pid) }
}

// ActivePIDs returns the PIDs of every live task subprocess.
func ActivePIDs() []int {
	var pids []int
	activePIDs.Range(func(k, _ any) bool {
		pids = append(pids, k.(int))
		return true
	})
	return pids
}
