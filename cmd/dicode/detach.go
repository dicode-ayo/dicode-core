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

// defaultConfigPath is where every dicode command looks for its config when
// none is named — the daemon's own flag default, which is what makes an
// auto-started daemon and the CLI that started it agree on one config.
const defaultConfigPath = "dicode.yaml"

const (
	// detachHandoverTimeout bounds a whole handover. It covers a graceful
	// shutdown — in-flight runs are canceled and their logs flushed first —
	// as well as the replacement's startup.
	detachHandoverTimeout = 45 * time.Second

	// detachGraceWindow is how long the replacement has to claim the socket
	// after the outgoing daemon is gone. Past that, nothing is coming and
	// waiting out detachHandoverTimeout only delays the bad news.
	detachGraceWindow = 10 * time.Second
)

// daemonPaths is everything the CLI needs to reach one daemon: its control
// socket and the log a detached one writes to. Both derive from the data
// directory the config names, so they are resolved together.
type daemonPaths struct {
	sock    ipc.ControlSocket
	logPath string
}

func daemonPathsFor(configPath string) daemonPaths {
	dataDir := cliDataDirFor(configPath)
	return daemonPaths{
		sock:    ipc.ControlSocketIn(dataDir),
		logPath: filepath.Join(dataDir, daemon.LogFileName),
	}
}

// cmdDaemonDetach implements `dicode daemon --detach`: leave a daemon running
// in the background and hand the terminal back.
//
// With a daemon already running, the restart is the daemon's own job: it is
// the only process that can release the control socket, the HTTP port and the
// database before its replacement wants them. We ask for the handoff over the
// control socket and wait for a different pid to answer.
func cmdDaemonDetach(configPath string, port int) error {
	paths := daemonPathsFor(configPath)

	if !paths.sock.Reachable() {
		// A socket file left behind by a daemon that died hard would stop the
		// new one binding.
		_ = os.Remove(paths.sock.Path)
		return startDetached(configPath, port, paths)
	}

	status, err := paths.sock.Ping()
	if err != nil {
		return err
	}
	if !status.Foreground {
		fmt.Printf("dicode: daemon already running in the background (pid %d); nothing to detach\n", status.PID)
		fmt.Printf("dicode: its output → %s\n", paths.logPath)
		return nil
	}
	if port != 0 {
		// The replacement inherits the running daemon's configuration whole;
		// --port only ever seeds a first run's onboarding.
		fmt.Fprintf(os.Stderr, "dicode: ignoring --port %d — the detached daemon keeps the "+
			"running daemon's config; change server.port and restart to move it\n", port)
	}

	outgoing, err := requestDetach(paths.sock)
	if err != nil {
		return err
	}
	fmt.Printf("dicode: asked the running daemon (pid %d) to hand over; waiting for it to stand down\n", outgoing)

	pid, err := waitDaemonHandover(paths.sock, outgoing, detachHandoverTimeout)
	if err != nil {
		return fmt.Errorf("%w (check %s)", err, paths.logPath)
	}
	daemon.PrintDetachedNotice(os.Stdout, pid, paths.logPath)
	return nil
}

// startDetached spawns a daemon directly, for the case where none is running.
// The pid reported is the one that answered on the socket: it is the process
// we spawned unless that one died on startup, and then the answering daemon is
// the one the user cares about.
func startDetached(configPath string, port int, paths daemonPaths) error {
	if _, err := daemon.SpawnDetached(configPath, port, paths.logPath); err != nil {
		return err
	}
	pid, err := daemon.WaitDaemonUp(paths.sock)
	if err != nil {
		return fmt.Errorf("%w (check %s)", err, paths.logPath)
	}
	daemon.PrintDetachedNotice(os.Stdout, pid, paths.logPath)
	return nil
}

// requestDetach asks the running daemon to restart itself detached, returning
// the pid it is about to give up.
func requestDetach(sock ipc.ControlSocket) (int, error) {
	c, err := sock.Dial()
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

// waitDaemonHandover blocks until a daemon other than the outgoing one answers
// on the control socket, and returns its pid.
//
// The pid is what makes the wait reliable: the outgoing daemon keeps answering
// until its listener closes and the replacement can claim the socket within
// one polling interval, so an absent socket is a state the caller may never
// observe. Watching the outgoing process is what separates a slow handover
// from one that is never completing.
func waitDaemonHandover(sock ipc.ControlSocket, outgoing int, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	var graceDeadline time.Time
	for {
		if status, err := sock.Ping(); err == nil && status.PID != outgoing {
			return status.PID, nil
		}
		switch {
		case daemon.ProcessAlive(outgoing):
		case graceDeadline.IsZero():
			graceDeadline = time.Now().Add(detachGraceWindow)
		case time.Now().After(graceDeadline):
			return 0, fmt.Errorf("the daemon (pid %d) exited without leaving a replacement", outgoing)
		}
		if !time.Now().Before(deadline) {
			return 0, fmt.Errorf("no detached daemon answered within %s", timeout)
		}
		time.Sleep(daemon.PingInterval)
	}
}
