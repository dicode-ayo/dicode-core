package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// LogFileName is the file inside the data directory that a detached daemon
// writes its stdout and stderr to. It is the only record of a background
// daemon's startup failures, and of the first-run dashboard passphrase, which
// pkg/webui prints once and then keeps only as a bcrypt hash.
const LogFileName = "daemon.log"

// SpawnDetached starts a fresh `dicode daemon` in its own session and returns
// its pid, with output appended to logPath. An empty logPath discards the
// output — a last resort, since it throws away both the startup diagnostics
// and a first run's dashboard passphrase.
//
// The child has no controlling terminal, so closing the terminal that spawned
// it no longer takes it down — the daemon treats SIGHUP as shutdown, which a
// child left in the terminal's session would receive. configPath and port are
// forwarded as the flags `dicode daemon` itself takes; port is omitted when
// zero, leaving the config's own value in force.
//
// The caller must not wait on the returned pid: the process is deliberately
// outliving it and is reparented to init once the caller exits.
func SpawnDetached(configPath string, port int, logPath string) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("resolve executable path: %w", err)
	}
	// 0o700/0o600 because of the first-run passphrase in the captured output.
	// MkdirAll because on a first run nothing has created the data dir yet.
	var logFile *os.File
	if logPath != "" {
		if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
			return 0, fmt.Errorf("create %s: %w", filepath.Dir(logPath), err)
		}
		logFile, err = os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return 0, fmt.Errorf("open %s: %w", logPath, err)
		}
		defer logFile.Close()
	}

	args := []string{"daemon"}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	if port != 0 {
		args = append(args, "--port", strconv.Itoa(port))
	}
	cmd := exec.Command(self, args...) // #nosec G204 — self is our own executable, args are typed flags.
	cmd.Stdin = nil
	if logFile != nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	}
	cmd.SysProcAttr = detachSysProcAttr()
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start daemon: %w", err)
	}
	pid := cmd.Process.Pid
	// Release, not Wait: nothing here will be alive to reap the child.
	_ = cmd.Process.Release()
	return pid, nil
}
