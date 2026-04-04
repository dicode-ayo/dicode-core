//go:build linux

package metrics

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// clkTck is the number of clock ticks per second on Linux (usually 100).
const clkTck = 100

// readSelfCPUMs reads the daemon's own CPU time (user+sys) from /proc/self/stat
// and returns it in milliseconds.
func readSelfCPUMs() int64 {
	return readProcStatCPUMs("/proc/self/stat")
}

// readProcCPUMs reads a child process's CPU time from /proc/<pid>/stat.
func readProcCPUMs(pid int) int64 {
	return readProcStatCPUMs(fmt.Sprintf("/proc/%d/stat", pid))
}

// readProcStatCPUMs parses a /proc/*/stat file and returns (utime+stime) in ms.
func readProcStatCPUMs(path string) int64 {
	data, err := os.ReadFile(path)
	if err != nil {
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
func readProcRSSMB(pid int) float64 {
	path := fmt.Sprintf("/proc/%d/status", pid)
	data, err := os.ReadFile(path)
	if err != nil {
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
