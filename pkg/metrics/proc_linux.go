//go:build linux

package metrics

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// clkTck is the number of clock ticks per second used when converting /proc
// stat jiffies to milliseconds. The value 100 is correct on the vast majority
// of Linux systems (CONFIG_HZ=100) but is not universal — kernels compiled
// with CONFIG_HZ=250 or CONFIG_HZ=1000 will produce scaled but not exact
// values. Querying the actual value via sysconf(_SC_CLK_TCK) would require
// cgo; the approximation is acceptable for monitoring purposes.
const clkTck = 100

// readSelfCPUMs reads the daemon's own CPU time (user+sys) from /proc/self/stat
// and returns it in milliseconds.
func readSelfCPUMs() int64 {
	return readProcStatCPUMs("/proc/self/stat")
}

// selfCPUMsPtr returns the daemon CPU time as a pointer so callers can
// distinguish "zero CPU" from "metric not available". On Linux this is always
// non-nil (may be zero if the process is brand new).
func selfCPUMsPtr() *int64 {
	v := readSelfCPUMs()
	return &v
}

// readProcCPUMs reads a child process's CPU time from /proc/<pid>/stat.
// If the process has exited (ENOENT or ESRCH) the function returns 0 silently.
func readProcCPUMs(pid int) int64 {
	return readProcStatCPUMs(fmt.Sprintf("/proc/%d/stat", pid))
}

// readProcStatCPUMs parses a /proc/*/stat file and returns (utime+stime) in ms.
// If the file does not exist (process exited / PID recycled) it returns 0.
func readProcStatCPUMs(path string) int64 {
	data, err := os.ReadFile(path)
	if err != nil {
		// ENOENT means the process has already exited; skip silently.
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		return 0
	}
	// The stat file has the process name in parentheses which may contain spaces.
	// Find the closing ')' and parse fields after it.
	line := strings.TrimSpace(string(data))
	rp := strings.LastIndex(line, ")")
	if rp < 0 {
		return 0
	}
	fields := strings.Fields(line[rp+1:])
	// After ')': field index 0 = state (field 3 in spec), fields 11 = utime (field 14), 12 = stime (field 15).
	// Fields are 0-indexed from the character after ')':
	//   0: state, 1: ppid, 2: pgrp, 3: session, 4: tty_nr, 5: tpgid,
	//   6: flags, 7: minflt, 8: cminflt, 9: majflt, 10: cmajflt,
	//   11: utime, 12: stime
	if len(fields) < 13 {
		return 0
	}
	utime, err1 := strconv.ParseInt(fields[11], 10, 64)
	stime, err2 := strconv.ParseInt(fields[12], 10, 64)
	if err1 != nil || err2 != nil {
		return 0
	}
	return (utime + stime) * 1000 / clkTck
}

// readProcRSSMB reads a process's Resident Set Size from /proc/<pid>/status.
// If the process has exited (file gone) it returns 0 silently.
func readProcRSSMB(pid int) float64 {
	path := fmt.Sprintf("/proc/%d/status", pid)
	data, err := os.ReadFile(path)
	if err != nil {
		// ENOENT: process exited between ActivePIDs() and this read — skip.
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, err := strconv.ParseFloat(fields[1], 64)
				if err == nil {
					return kb / 1024
				}
			}
		}
	}
	return 0
}
