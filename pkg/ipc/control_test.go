package ipc

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
)

// controlTestEnv spins up a ControlServer listening on a temp Unix socket and
// returns an authenticated net.Conn ready for request/response exchanges.
func controlTestEnv(t *testing.T, mp MetricsProvider) (net.Conn, func()) {
	t.Helper()

	dir := t.TempDir()
	socketPath := filepath.Join(dir, "ctrl.sock")
	tokenPath := filepath.Join(dir, "ctrl.token")

	log := zap.NewNop()
	eng := &mockEngine{}

	cs, err := NewControlServer(socketPath, tokenPath, nil, eng, nil, mp, "test", log)
	if err != nil {
		t.Fatalf("NewControlServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = cs.Start(ctx)
	}()

	// Wait for the socket file to appear (Start is fast but runs in a goroutine).
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("control socket never appeared within 2s")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Read the token the server wrote.
	tok, err := os.ReadFile(tokenPath)
	if err != nil {
		cancel()
		t.Fatalf("read token: %v", err)
	}

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		cancel()
		t.Fatalf("dial control: %v", err)
	}

	// Handshake.
	if err := writeMsg(conn, handshakeReq{Token: string(tok)}); err != nil {
		conn.Close()
		cancel()
		t.Fatalf("handshake send: %v", err)
	}
	var hs struct {
		Proto int      `json:"proto"`
		Caps  []string `json:"caps"`
		Error string   `json:"error"`
	}
	if err := readMsg(conn, &hs); err != nil {
		conn.Close()
		cancel()
		t.Fatalf("handshake recv: %v", err)
	}
	if hs.Error != "" {
		conn.Close()
		cancel()
		t.Fatalf("handshake rejected: %s", hs.Error)
	}

	cleanup := func() {
		conn.Close()
		cancel()
		<-done
	}
	return conn, cleanup
}

func TestControl_Metrics_ReturnsSnapshot(t *testing.T) {
	t.Parallel()

	cpuMs := int64(1234)
	mp := MetricsProvider{
		ReadDaemon: func() (float64, float64, int, *int64) {
			return 42.5, 64.0, 17, &cpuMs
		},
		ActivePIDs: func() []int { return []int{1234, 5678} },
		ReadChildren: func(pids []int, active int) (float64, *int64) {
			total := int64(int64(len(pids)) * 100)
			return float64(len(pids)) * 50.0, &total
		},
	}

	conn, cleanup := controlTestEnv(t, mp)
	defer cleanup()

	if err := writeMsg(conn, Request{ID: "m1", Method: "cli.metrics"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	var resp struct {
		ID     string          `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	if err := readMsg(conn, &resp); err != nil {
		t.Fatalf("recv: %v", err)
	}
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}

	var snap MetricsSnapshot
	if err := json.Unmarshal(resp.Result, &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}

	if snap.Daemon.HeapAllocMB != 42.5 {
		t.Errorf("HeapAllocMB: got %v, want 42.5", snap.Daemon.HeapAllocMB)
	}
	if snap.Daemon.Goroutines != 17 {
		t.Errorf("Goroutines: got %v, want 17", snap.Daemon.Goroutines)
	}
	if snap.Daemon.CPUMs == nil || *snap.Daemon.CPUMs != 1234 {
		t.Errorf("CPUMs: got %v, want &1234", snap.Daemon.CPUMs)
	}
	if snap.Tasks.ActiveTasks != 0 { // mockEngine returns 0
		t.Errorf("ActiveTasks: got %v, want 0", snap.Tasks.ActiveTasks)
	}
	if snap.Tasks.ChildRSSMB != 100.0 {
		t.Errorf("ChildRSSMB: got %v, want 100.0", snap.Tasks.ChildRSSMB)
	}
}

func TestControl_Metrics_NoProvider_ReturnsZeros(t *testing.T) {
	t.Parallel()

	// Empty MetricsProvider — all function fields nil.
	conn, cleanup := controlTestEnv(t, MetricsProvider{})
	defer cleanup()

	if err := writeMsg(conn, Request{ID: "m2", Method: "cli.metrics"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	var resp struct {
		ID     string          `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	if err := readMsg(conn, &resp); err != nil {
		t.Fatalf("recv: %v", err)
	}
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}

	var snap MetricsSnapshot
	if err := json.Unmarshal(resp.Result, &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// All daemon fields should be zero-value; no panic.
	if snap.Daemon.Goroutines != 0 {
		t.Errorf("expected zero goroutines without provider, got %d", snap.Daemon.Goroutines)
	}
}

func TestControl_UnknownMethod_ReturnsError(t *testing.T) {
	t.Parallel()

	conn, cleanup := controlTestEnv(t, MetricsProvider{})
	defer cleanup()

	if err := writeMsg(conn, Request{ID: "u1", Method: "cli.does_not_exist"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	var resp struct {
		ID    string `json:"id"`
		Error string `json:"error"`
	}
	if err := readMsg(conn, &resp); err != nil {
		t.Fatalf("recv: %v", err)
	}
	if resp.Error == "" {
		t.Error("expected error for unknown method, got none")
	}
}
