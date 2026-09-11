package ipc

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/registry"
	"go.uber.org/zap"
)

// Tests for cli.daemon.detach — the verb `dicode daemon --detach` uses to ask
// a foreground daemon to restart itself in the background.

func TestControl_Detach_RefusedWithoutHandoff(t *testing.T) {
	t.Parallel()
	cs := &ControlServer{log: zap.NewNop()} // no SetDetach — tests / stripped builds

	if _, err := cs.handleDaemonDetach(); err == nil {
		t.Fatal("a control server with no handoff wired must refuse the verb, not acknowledge it")
	}
}

func TestControl_Detach_ReportsOutgoingPID(t *testing.T) {
	t.Parallel()
	cs := &ControlServer{log: zap.NewNop()}
	cs.SetDetach(func() {})
	cs.SetDaemonProcess(4242, true)

	res, err := cs.handleDaemonDetach()
	if err != nil {
		t.Fatalf("handleDaemonDetach: %v", err)
	}
	if res.PID != 4242 {
		t.Fatalf("detach result pid = %d, want 4242 — the caller tells the replacement apart by it", res.PID)
	}
}

// TestControl_Detach_AcknowledgesBeforeHandoff pins the ordering the CLI
// depends on: the handoff tears the listener down, so a detach started from
// inside dispatch would reach the caller as a dropped connection instead of a
// reply. It also covers the process identity cli.ping reports, which is how
// the CLI decides whether a detach is needed at all.
func TestControl_Detach_AcknowledgesBeforeHandoff(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	socketPath := filepath.Join(dir, "ctrl.sock")
	tokenPath := filepath.Join(dir, "ctrl.token")

	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	cs, err := NewControlServer(socketPath, tokenPath, registry.New(d), &mockEngine{}, nil, MetricsProvider{}, "test", zap.NewNop(), nil, "", "")
	if err != nil {
		t.Fatalf("NewControlServer: %v", err)
	}
	cs.SetDaemonProcess(4242, true)

	// Stand in for the real handoff, which shuts the daemon down: record that
	// it ran, which must be after the reply is on the wire.
	handoff := make(chan struct{})
	cs.SetDetach(func() { close(handoff) })

	defer serveControl(t, cs, socketPath)()
	conn := dialControl(t, socketPath, tokenPath)
	defer conn.Close()

	var status DaemonStatus
	sendControl(t, conn, Request{ID: "1", Method: "cli.ping"}, &status)
	if status.PID != 4242 || !status.Foreground {
		t.Fatalf("cli.ping reported pid=%d foreground=%v, want the values SetDaemonProcess was given",
			status.PID, status.Foreground)
	}

	var res DaemonDetachResult
	sendControl(t, conn, Request{ID: "2", Method: MethodDaemonDetach}, &res)
	if res.PID != 4242 {
		t.Fatalf("detach result pid = %d, want 4242", res.PID)
	}

	select {
	case <-handoff:
	case <-time.After(2 * time.Second):
		t.Fatal("handoff never ran after the ack")
	}
}
