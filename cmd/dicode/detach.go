package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/dicode/dicode/pkg/daemon"
	"github.com/dicode/dicode/pkg/ipc"
)

// defaultConfigPath is where every dicode command looks for its config when
// none is named — the daemon's own flag default, which is what makes an
// auto-started daemon and the CLI that started it agree on one config.
const defaultConfigPath = "dicode.yaml"

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

// cmdDaemonDetach implements `dicode daemon --detach`: start the daemon in the
// background and hand the terminal back.
func cmdDaemonDetach(configPath string, port int) error {
	paths := daemonPathsFor(configPath)

	if paths.sock.Reachable() {
		fmt.Printf("dicode: a daemon is already running; nothing to start\n")
		fmt.Printf("dicode: its output → %s\n", paths.logPath)
		return nil
	}
	// A socket file left behind by a daemon that died hard would stop the new
	// one binding.
	_ = os.Remove(paths.sock.Path)

	pid, err := daemon.SpawnDetached(configPath, port, paths.logPath)
	if err != nil {
		return err
	}
	if err := daemon.WaitDaemonUp(paths.sock); err != nil {
		return fmt.Errorf("%w (check %s)", err, paths.logPath)
	}
	fmt.Printf("dicode: daemon detached — pid %d\n", pid)
	// The log is where a background daemon's startup errors land, and where a
	// first run prints the dashboard passphrase, which is shown once and then
	// kept only as a bcrypt hash.
	fmt.Printf("dicode: output → %s\n", paths.logPath)
	fmt.Printf("dicode: stop it with `kill %d`\n", pid)
	return nil
}
