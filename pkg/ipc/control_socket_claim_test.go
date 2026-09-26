package ipc

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	"go.uber.org/zap"
)

// startControlServer brings a ControlServer up on dir's socket and returns the
// socket path once it is accepting connections.
func startControlServer(t *testing.T, dir string) (string, context.CancelFunc) {
	t.Helper()

	socketPath := filepath.Join(dir, "daemon.sock")
	tokenPath := filepath.Join(dir, "daemon.token")

	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	cs, err := NewControlServer(socketPath, tokenPath, registry.New(d), &mockEngine{}, nil, MetricsProvider{}, "test", zap.NewNop(), nil, "", "")
	if err != nil {
		t.Fatalf("NewControlServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = cs.Start(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if conn, err := net.Dial("unix", socketPath); err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("control socket never came up within 2s")
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(cancel)
	return socketPath, cancel
}

// TestControlServerStart_RefusesALiveSocket: a second daemon aimed at a data
// directory that already has one must not take the socket over. Start unlinks
// a stale socket before binding, and an unconditional unlink lets the newcomer
// bind the same path — two processes on one data directory, with the CLI
// reaching whichever won.
func TestControlServerStart_RefusesALiveSocket(t *testing.T) {
	dir := t.TempDir()
	socketPath, _ := startControlServer(t, dir)

	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	defer d.Close()

	second, err := NewControlServer(socketPath, filepath.Join(dir, "daemon.token"), registry.New(d), &mockEngine{}, nil, MetricsProvider{}, "test", zap.NewNop(), nil, "", "")
	if err != nil {
		t.Fatalf("NewControlServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err = second.Start(ctx)
	if err == nil {
		t.Fatal("second Start() succeeded; it took over a live socket")
	}
	if !strings.Contains(err.Error(), "already") {
		t.Errorf("Start() error = %v; want it to name the running daemon", err)
	}

	// The first daemon is still the one listening.
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("original daemon no longer reachable: %v", err)
	}
	conn.Close()
}

// TestControlServerStart_ReplacesAStaleSocket: a socket file left behind by a
// daemon that died hard must not stop the next one binding.
func TestControlServerStart_ReplacesAStaleSocket(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "daemon.sock")
	if err := os.WriteFile(socketPath, nil, 0o600); err != nil {
		t.Fatalf("plant stale socket: %v", err)
	}

	startControlServer(t, dir)
}
