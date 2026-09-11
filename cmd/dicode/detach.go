package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dicode/dicode/pkg/daemon"
	"github.com/dicode/dicode/pkg/ipc"
)

// detachHandoffTimeout bounds the wait for a running daemon to hand over to
// its detached replacement. It covers a full graceful shutdown — in-flight
// task runs are canceled and their logs flushed first — plus the
// replacement's startup, so it is far more generous than a cold start alone.
const detachHandoffTimeout = 45 * time.Second

// cmdDaemonDetach implements `dicode daemon --detach`: leave a daemon running
// in the background and hand the terminal back.
//
// With a daemon already running, the restart is the daemon's own job rather
// than ours: it is the only process that can release the control socket, the
// HTTP port and the database before its replacement wants them. We ask for
// the handoff over the control socket and wait for a different pid to answer.
func cmdDaemonDetach(configPath string, port int) error {
	dataDir := cliDataDirFor(configPath)
	socketPath := filepath.Join(dataDir, "daemon.sock")
	tokenPath := filepath.Join(dataDir, "daemon.token")
	logPath := filepath.Join(dataDir, daemon.LogFileName)

	if !isDaemonRunning(socketPath) {
		// A socket file left behind by a daemon that died hard would stop the
		// new one binding.
		_ = os.Remove(socketPath)
		return startDetached(configPath, port, socketPath, tokenPath, logPath)
	}

	status, err := daemonPing(socketPath, tokenPath)
	if err != nil {
		return err
	}
	if !status.Foreground {
		fmt.Printf("dicode: daemon already running in the background (pid %d); nothing to detach\n", status.PID)
		fmt.Printf("dicode: its output → %s\n", logPath)
		return nil
	}

	oldPID, err := requestDetach(socketPath, tokenPath)
	if err != nil {
		return err
	}
	fmt.Printf("dicode: asked the running daemon (pid %d) to hand over; waiting for it to stand down\n", oldPID)

	newPID, err := waitDaemonHandoff(socketPath, tokenPath, oldPID, detachHandoffTimeout)
	if err != nil {
		return fmt.Errorf("%w (the daemon may be down — check %s)", err, logPath)
	}
	printDetached(newPID, logPath)
	return nil
}

// startDetached spawns a daemon directly, for the case where none is running.
// The pid reported is the one that answered on the socket rather than the one
// we spawned: they are the same process unless the spawn died on startup, and
// then the answering daemon is the one the user cares about.
func startDetached(configPath string, port int, socketPath, tokenPath, logPath string) error {
	if _, err := daemon.SpawnDetached(absPath(configPath), port, logPath); err != nil {
		return err
	}
	pid, err := waitDaemonHandoff(socketPath, tokenPath, 0, detachHandoffTimeout)
	if err != nil {
		return fmt.Errorf("%w (check %s)", err, logPath)
	}
	printDetached(pid, logPath)
	return nil
}

func printDetached(pid int, logPath string) {
	fmt.Printf("dicode: daemon detached — pid %d\n", pid)
	// The log is where a background daemon's startup errors land, and where a
	// first run prints the dashboard passphrase, which is shown once and then
	// kept only as a bcrypt hash.
	fmt.Printf("dicode: output → %s\n", logPath)
	fmt.Printf("dicode: stop it with `kill %d`\n", pid)
}

// requestDetach asks the running daemon to restart itself detached, returning
// the pid it is about to give up.
func requestDetach(socketPath, tokenPath string) (int, error) {
	c, err := ipc.Dial(socketPath, tokenPath)
	if err != nil {
		return 0, fmt.Errorf("connect to daemon: %w", err)
	}
	defer c.Close()

	resp, err := c.Send(ipc.Request{Method: ipc.MethodDaemonDetach})
	if err != nil {
		return 0, fmt.Errorf("ask daemon to detach: %w", err)
	}
	if resp.Error != "" {
		if strings.Contains(resp.Error, "unknown method") {
			return 0, fmt.Errorf("the running daemon is too old to detach itself — stop it and run `dicode daemon --detach` again")
		}
		return 0, fmt.Errorf("ask daemon to detach: %s", resp.Error)
	}
	var r ipc.DaemonDetachResult
	if err := remarshal(resp.Result, &r); err != nil {
		return 0, fmt.Errorf("decode detach result: %w", err)
	}
	return r.PID, nil
}

// waitDaemonHandoff blocks until a daemon whose pid is not oldPID answers on
// the control socket, and returns that pid. Pass 0 as oldPID to wait for any
// daemon.
//
// The pid is what makes the wait reliable: the outgoing daemon keeps
// answering until its listener closes, and the replacement may claim the
// socket within the same polling interval, so "the socket went away" is a
// state a caller can easily never observe.
func waitDaemonHandoff(socketPath, tokenPath string, oldPID int, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	for {
		if status, err := daemonPing(socketPath, tokenPath); err == nil && status.PID != oldPID {
			return status.PID, nil
		}
		if !time.Now().Before(deadline) {
			return 0, fmt.Errorf("no detached daemon answered within %s", timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// daemonPing reads the running daemon's status off the control socket.
func daemonPing(socketPath, tokenPath string) (ipc.DaemonStatus, error) {
	c, err := ipc.Dial(socketPath, tokenPath)
	if err != nil {
		return ipc.DaemonStatus{}, fmt.Errorf("connect to daemon: %w", err)
	}
	defer c.Close()

	resp, err := c.Send(ipc.Request{Method: "cli.ping"})
	if err != nil {
		return ipc.DaemonStatus{}, fmt.Errorf("query daemon: %w", err)
	}
	if resp.Error != "" {
		return ipc.DaemonStatus{}, fmt.Errorf("query daemon: %s", resp.Error)
	}
	var status ipc.DaemonStatus
	if err := remarshal(resp.Result, &status); err != nil {
		return ipc.DaemonStatus{}, fmt.Errorf("decode daemon status: %w", err)
	}
	return status, nil
}

// absPath resolves a path against the current directory, passing an
// unresolvable one through for the daemon to complain about in its own terms.
// A detached daemon is handed its config path on the command line and may be
// started from somewhere else entirely.
func absPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}
