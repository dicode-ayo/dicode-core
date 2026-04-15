// Package metrics collects runtime and process metrics for the daemon.
package metrics

import (
	"runtime"
	"runtime/metrics"
	"sync"
	"time"
)

// DaemonMetrics holds Go-runtime-level metrics for the daemon process.
type DaemonMetrics struct {
	HeapAllocMB float64 `json:"heap_alloc_mb"`
	HeapSysMB   float64 `json:"heap_sys_mb"`
	Goroutines  int     `json:"goroutines"`
	// CPUMs is the daemon's cumulative CPU time (user+sys) in milliseconds.
	// It uses a pointer so that zero is a valid value and nil means
	// "not available" (non-Linux). omitempty omits the field when nil.
	CPUMs *int64 `json:"cpu_ms,omitempty"` // Linux only
}

// ChildMetrics holds aggregate metrics across all active Deno child processes.
type ChildMetrics struct {
	ActiveTasks int     `json:"active_tasks"`
	ChildRSSMB  float64 `json:"children_rss_mb,omitempty"` // Linux only
	// ChildCPUMs is the aggregate CPU time of all active child processes in ms.
	// Pointer so zero CPU is preserved and nil indicates non-Linux.
	ChildCPUMs *int64 `json:"children_cpu_ms,omitempty"` // Linux only

	// ActiveTaskSlots is the number of semaphore slots currently held by
	// running task goroutines. 0 when no concurrency cap is configured.
	ActiveTaskSlots int `json:"active_task_slots"`
	// MaxConcurrentTasks is the configured concurrency cap. 0 = unlimited.
	MaxConcurrentTasks int `json:"max_concurrent_tasks"`
	// WaitingTasks is the number of task goroutines parked waiting for a
	// free semaphore slot. Always 0 when no cap is configured.
	WaitingTasks int `json:"waiting_tasks"`
}

// Metrics is the top-level response for GET /api/metrics.
type Metrics struct {
	Daemon DaemonMetrics `json:"daemon"`
	Tasks  ChildMetrics  `json:"tasks"`
}

// metricsCache holds a cached snapshot to avoid repeated sampling within a
// short window. It is protected by mu.
var (
	mu          sync.Mutex
	cachedAt    time.Time
	cachedValue DaemonMetrics
)

const cacheTTL = 5 * time.Second

// ReadDaemonMetrics returns current daemon metrics using the runtime/metrics
// package (no stop-the-world GC pause). Results are cached for up to 5 seconds
// to prevent burst abuse.
func ReadDaemonMetrics() DaemonMetrics {
	mu.Lock()
	defer mu.Unlock()

	if time.Since(cachedAt) < cacheTTL {
		return cachedValue
	}

	// runtime/metrics.Read does NOT trigger a stop-the-world pause, unlike
	// runtime.ReadMemStats which halts all goroutines while copying MemStats.
	samples := []metrics.Sample{
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/memory/classes/total:bytes"},
	}
	metrics.Read(samples)
	heapAlloc := samples[0].Value.Uint64()
	heapSys := samples[1].Value.Uint64()

	d := DaemonMetrics{
		HeapAllocMB: float64(heapAlloc) / (1024 * 1024),
		HeapSysMB:   float64(heapSys) / (1024 * 1024),
		Goroutines:  runtime.NumGoroutine(),
		CPUMs:       selfCPUMsPtr(), // nil on non-Linux; &0 is a valid zero value
	}

	cachedValue = d
	cachedAt = time.Now()
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
		c.ChildCPUMs = &totalCPU
	}
	return c
}
