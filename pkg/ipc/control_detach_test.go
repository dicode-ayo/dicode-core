package ipc

import (
	"context"
	"encoding/json"
	"net"
	"os"
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
	cs.SetDaemonProcess(4242, "/somewhere/dicode.yaml", true)

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
	configPath := filepath.Join(dir, "dicode.yaml")

	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	cs, err := NewControlServer(socketPath, tokenPath, registry.New(d), &mockEngine{}, nil, MetricsProvider{}, "test", zap.NewNop(), nil, "", "")
	if err != nil {
		t.Fatalf("NewControlServer: %v", err)
	}
	cs.SetDaemonProcess(4242, configPath, true)

	// Stand in for the real handoff, which shuts the daemon down: record that
	// it ran, which must be after the reply is on the wire.
	handoff := make(chan struct{})
	cs.SetDetach(func() { close(handoff) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = cs.Start(ctx)
	}()
	defer func() { cancel(); <-served }()
	waitForSocket(t, socketPath)

	conn := dialControl(t, socketPath, tokenPath)
	defer conn.Close()

	var status DaemonStatus
	sendControl(t, conn, Request{ID: "1", Method: "cli.ping"}, &status)
	if status.PID != 4242 || status.ConfigPath != configPath || !status.Foreground {
		t.Fatalf("cli.ping reported pid=%d config=%q foreground=%v, want the values SetDaemonProcess was given",
			status.PID, status.ConfigPath, status.Foreground)
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

func waitForSocket(t *testing.T, socketPath string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(socketPath); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("control socket never appeared within 2s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func dialControl(t *testing.T, socketPath, tokenPath string) net.Conn {
	t.Helper()
	tok, err := readCLITokenFile(tokenPath)
	if err != nil {
		t.Fatalf("read token: %v", err)
	}
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial control: %v", err)
	}
	if err := writeMsg(conn, handshakeReq{Token: string(tok)}); err != nil {
		conn.Close()
		t.Fatalf("handshake send: %v", err)
	}
	var hs struct {
		Proto int      `json:"proto"`
		Error string   `json:"error"`
		Caps  []string `json:"caps"`
	}
	if err := readMsg(conn, &hs); err != nil {
		conn.Close()
		t.Fatalf("handshake recv: %v", err)
	}
	if hs.Error != "" {
		conn.Close()
		t.Fatalf("handshake rejected: %s", hs.Error)
	}
	return conn
}

// sendControl round-trips one request and decodes the result into out.
func sendControl(t *testing.T, conn net.Conn, req Request, out any) {
	t.Helper()
	if err := writeMsg(conn, req); err != nil {
		t.Fatalf("send %s: %v", req.Method, err)
	}
	var resp Response
	if err := readMsg(conn, &resp); err != nil {
		t.Fatalf("no reply to %s: %v", req.Method, err)
	}
	if resp.Error != "" {
		t.Fatalf("%s refused: %s", req.Method, resp.Error)
	}
	raw, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("re-encode %s result: %v", req.Method, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("decode %s result: %v", req.Method, err)
	}
}
