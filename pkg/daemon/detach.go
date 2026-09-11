package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/dicode/dicode/pkg/ipc"
)

// LogFileName is the file inside the data directory that a detached daemon
// writes its stdout and stderr to. It is the only record of a background
// daemon's startup failures, and of the first-run dashboard passphrase, which
// pkg/webui prints once and then keeps only as a bcrypt hash.
const LogFileName = "daemon.log"

// startTimeout bounds the wait for a freshly spawned daemon to claim the
// control socket.
const startTimeout = 15 * time.Second

// SpawnDetached starts a fresh `dicode daemon` in its own session and returns
// its pid, with output appended to logPath. Pass os.DevNull to discard the
// output, losing the startup diagnostics and any first-run passphrase with it.
//
// The child has no controlling terminal, so closing the terminal that spawned
// it no longer takes it down — the daemon treats SIGHUP as shutdown, which a
// child left in the terminal's session would receive. configPath and port are
// forwarded as the flags `dicode daemon` itself takes; a zero port leaves the
// config's own value in force. configPath is resolved against the current
// directory, since the child may be started from somewhere else entirely.
//
// The caller must not wait on the returned pid: the process outlives it and is
// reparented to init, which reaps it.
func SpawnDetached(configPath string, port int, logPath string) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("resolve executable path: %w", err)
	}
	// 0o700/0o600 because of the first-run passphrase in the captured output.
	// MkdirAll because on a first run nothing has created the data dir yet.
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return 0, fmt.Errorf("create %s: %w", filepath.Dir(logPath), err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", logPath, err)
	}
	defer logFile.Close()

	args := []string{"daemon", "--config", absConfigPath(configPath)}
	if port != 0 {
		args = append(args, "--port", strconv.Itoa(port))
	}
	cmd := exec.Command(self, args...) // #nosec G204 — self is our own executable, args are typed flags.
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = detachSysProcAttr()
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start daemon: %w", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

// absConfigPath resolves configPath against the current directory, passing an
// unresolvable one through for the daemon to complain about in its own terms.
func absConfigPath(configPath string) string {
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return configPath
	}
	return abs
}

// PingInterval is how often the control socket is probed while waiting for a
// daemon to appear or to hand over.
const PingInterval = 200 * time.Millisecond

// WaitDaemonUp blocks until a daemon is accepting on the control socket, or
// gives up once a cold start's worth of time has passed.
func WaitDaemonUp(sock ipc.ControlSocket) error {
	deadline := time.Now().Add(startTimeout)
	for {
		if sock.Reachable() {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("no daemon answered within %s", startTimeout)
		}
		time.Sleep(PingInterval)
	}
}
