// Package metrics collects runtime and process metrics for the daemon.
package metrics

import "runtime"

// DaemonMetrics holds Go-runtime-level metrics for the daemon process.
type DaemonMetrics struct {
	HeapAllocMB float64 `json:"heap_alloc_mb"`
	HeapSysMB   float64 `json:"heap_sys_mb"`
	Goroutines  int     `json:"goroutines"`
	CPUMs       int64   `json:"cpu_ms,omitempty"` // Linux only
}

// ChildMetrics holds aggregate metrics across all active Deno child processes.
type ChildMetrics struct {
	ActiveTasks int     `json:"active_tasks"`
	ChildRSSMB  float64 `json:"children_rss_mb,omitempty"` // Linux only
	ChildCPUMs  int64   `json:"children_cpu_ms,omitempty"` // Linux only
}

// Metrics is the top-level response for GET /api/metrics.
type Metrics struct {
	Daemon DaemonMetrics `json:"daemon"`
	Tasks  ChildMetrics  `json:"tasks"`
}

// ReadDaemonMetrics returns current daemon metrics using runtime.ReadMemStats.
func ReadDaemonMetrics() DaemonMetrics {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	d := DaemonMetrics{
		HeapAllocMB: float64(ms.HeapAlloc) / (1024 * 1024),
		HeapSysMB:   float64(ms.HeapSys) / (1024 * 1024),
		Goroutines:  runtime.NumGoroutine(),
	}
	d.CPUMs = readSelfCPUMs()
	return d
}

// ReadChildMetrics returns aggregate metrics for the provided set of child PIDs.
func ReadChildMetrics(pids []int, activeTasks int) ChildMetrics {
	c := ChildMetrics{ActiveTasks: activeTasks}
	var totalRSS float64
	var totalCPU int64
	for _, pid := range pids {
		rss := readProcRSSMB(pid)
		cpu := readProcCPUMs(pid)
		totalRSS += rss
		totalCPU += cpu
	}
	if len(pids) > 0 {
		c.ChildRSSMB = totalRSS
		c.ChildCPUMs = totalCPU
	}
	return c
}
