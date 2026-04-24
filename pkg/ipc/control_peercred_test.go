//go:build linux

package ipc

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestControlServerPeerCred proves the core security property: on Linux, the
// CLI can authenticate to the daemon control socket with NO TOKEN FILE on disk.
// SO_PEERCRED alone gates access. This test exists to prevent a silent
// regression in which the daemon begins writing the token again (re-introducing
// the on-disk attack surface).
func TestControlServerPeerCred(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "ctrl.sock")
	tokenPath := filepath.Join(dir, "ctrl.token")

	cs, err := NewControlServer(socketPath, tokenPath, nil, &mockEngine{}, nil, MetricsProvider{}, "test", zap.NewNop(), nil, "")
	if err != nil {
		t.Fatalf("NewControlServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = cs.Start(ctx) }()

	// Wait for socket.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("socket never appeared")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Core assertion: the token file must NOT exist on disk on Linux.
	if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Fatalf("token file unexpectedly present at %s (err=%v) — SO_PEERCRED should make it unnecessary", tokenPath, err)
	}

	// Dial and perform a handshake with an EMPTY token. The server must
	// accept via SO_PEERCRED (same process UID) and return proto=1 + caps.
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := writeMsg(conn, handshakeReq{Token: ""}); err != nil {
		t.Fatalf("handshake send: %v", err)
	}
	var hs struct {
		Proto int      `json:"proto"`
		Caps  []string `json:"caps"`
		Error string   `json:"error"`
	}
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if err := readMsg(conn, &hs); err != nil {
		t.Fatalf("handshake recv: %v", err)
	}
	if hs.Error != "" {
		t.Fatalf("server rejected same-uid peer: %s", hs.Error)
	}
	if hs.Proto != 1 {
		t.Fatalf("proto=%d, want 1", hs.Proto)
	}
	if len(hs.Caps) == 0 {
		t.Fatalf("server returned no caps; expected cliCaps() set")
	}
}
