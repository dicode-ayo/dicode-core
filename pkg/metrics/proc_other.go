//go:build !linux

package metrics

// readSelfCPUMs returns 0 on non-Linux platforms.
func readSelfCPUMs() int64 { return 0 }

// selfCPUMsPtr returns nil on non-Linux platforms to indicate the metric is
// not available — distinguishable from a real zero-CPU value.
func selfCPUMsPtr() *int64 { return nil }

// readProcCPUMs returns 0 on non-Linux platforms.
func readProcCPUMs(_ int) int64 { return 0 }

// readProcRSSMB returns 0 on non-Linux platforms.
func readProcRSSMB(_ int) float64 { return 0 }
