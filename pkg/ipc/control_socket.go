package ipc

import (
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"time"
)

// Fixed names inside the daemon's data directory. The daemon listens at one
// and writes the CLI's token to the other, and a CLI that resolves either to a
// different path is talking to nobody — so ControlSocketIn is the one place
// they are spelled.
const (
	socketFileName = "daemon.sock"
	tokenFileName  = "daemon.token"
)

// ControlSocket addresses one daemon's control socket: where to connect, and
// the token file that authenticates the connection. The two always travel
// together — Dial needs both, and on platforms without SO_PEERCRED the token
// is the only credential.
type ControlSocket struct {
	Path      string
	TokenPath string
}

// ControlSocketIn locates the control socket of the daemon whose data
// directory is dataDir.
func ControlSocketIn(dataDir string) ControlSocket {
	return ControlSocket{
		Path:      filepath.Join(dataDir, socketFileName),
		TokenPath: filepath.Join(dataDir, tokenFileName),
	}
}

// Dial connects and authenticates, as Dial(socketPath, tokenPath) does.
func (s ControlSocket) Dial() (*ControlClient, error) { return Dial(s.Path, s.TokenPath) }

// Reachable reports whether something accepts connections at the socket. It
// performs no handshake, so it says nothing about which daemon is listening.
func (s ControlSocket) Reachable() bool {
	conn, err := net.DialTimeout("unix", s.Path, 200*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Ping opens a one-shot connection and reports the daemon's status.
func (s ControlSocket) Ping() (DaemonStatus, error) {
	c, err := s.Dial()
	if err != nil {
		return DaemonStatus{}, fmt.Errorf("connect to daemon: %w", err)
	}
	defer c.Close()

	resp, err := c.Send(Request{Method: "cli.ping"})
	if err != nil {
		return DaemonStatus{}, fmt.Errorf("query daemon: %w", err)
	}
	if resp.Error != "" {
		return DaemonStatus{}, fmt.Errorf("query daemon: %s", resp.Error)
	}
	raw, err := json.Marshal(resp.Result)
	if err != nil {
		return DaemonStatus{}, fmt.Errorf("re-encode daemon status: %w", err)
	}
	var status DaemonStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		return DaemonStatus{}, fmt.Errorf("decode daemon status: %w", err)
	}
	return status, nil
}
