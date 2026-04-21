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
	"github.com/dicode/dicode/pkg/task"
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

	cs, err := NewControlServer(socketPath, tokenPath, nil, eng, nil, mp, "test", log, nil, "")
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

// The CLI control channel has no task context, so its handshake response
// must carry empty task_id / run_id strings — but the fields must be
// PRESENT on the wire so the shim-side decoder always sees them. This
// pairs with the non-omitempty encoding in message.go:handshakeResp.
//
// The "Emits" name is load-bearing: a prior name ("Omits…") suggested the
// fields could be absent and readers would "fix" it by asserting absence,
// which is the opposite of what we want.
func TestControl_Handshake_EmitsEmptyTaskAndRunIDFields(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	socketPath := filepath.Join(dir, "ctrl.sock")
	tokenPath := filepath.Join(dir, "ctrl.token")

	cs, err := NewControlServer(socketPath, tokenPath, nil, &mockEngine{}, nil, MetricsProvider{}, "test", zap.NewNop(), nil, "")
	if err != nil {
		t.Fatalf("NewControlServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = cs.Start(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("control socket never appeared")
		}
		time.Sleep(5 * time.Millisecond)
	}

	tok, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("read token: %v", err)
	}

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := writeMsg(conn, handshakeReq{Token: string(tok)}); err != nil {
		t.Fatalf("handshake send: %v", err)
	}
	// Decode as a map to prove the fields are present, even if empty.
	var raw map[string]any
	if err := readMsg(conn, &raw); err != nil {
		t.Fatalf("handshake recv: %v", err)
	}
	if errMsg, ok := raw["error"].(string); ok && errMsg != "" {
		t.Fatalf("handshake rejected: %s", errMsg)
	}

	// Keys must exist (non-omitempty encoding) and be empty strings
	// (no task context on the control channel).
	got, ok := raw["task_id"]
	if !ok {
		t.Errorf("handshake response missing task_id field; expected empty string")
	} else if got != "" {
		t.Errorf("control handshake task_id: got %q, want empty", got)
	}
	got, ok = raw["run_id"]
	if !ok {
		t.Errorf("handshake response missing run_id field; expected empty string")
	} else if got != "" {
		t.Errorf("control handshake run_id: got %q, want empty", got)
	}
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

// TestControl_AI_FiresConfiguredTask_AndReturnsReply is the happy path for
// `dicode ai`: the control server reads cfg.AI.Task, fires the engine, and
// extracts {session_id, reply} from the run's return value.
func TestControl_AI_FiresConfiguredTask_AndReturnsReply(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	socketPath := filepath.Join(dir, "ctrl.sock")
	tokenPath := filepath.Join(dir, "ctrl.token")

	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	defer d.Close()
	reg := registry.New(d)
	if err := reg.Register(&task.Spec{ID: "buildin/dicodai", Name: "dicodai"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	eng := &mockEngine{
		runID: "run-ai-1",
		result: RunResult{
			RunID:  "run-ai-1",
			Status: "success",
			ReturnValue: map[string]any{
				"session_id": "sess-abc",
				"reply":      "hello from the agent",
			},
		},
	}

	cs, err := NewControlServer(socketPath, tokenPath, reg, eng, nil, MetricsProvider{}, "test", zap.NewNop(), nil, "buildin/dicodai")
	if err != nil {
		t.Fatalf("NewControlServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = cs.Start(ctx) }()

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

	tok, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("read token: %v", err)
	}
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := writeMsg(conn, handshakeReq{Token: string(tok)}); err != nil {
		t.Fatalf("handshake send: %v", err)
	}
	var hs map[string]any
	if err := readMsg(conn, &hs); err != nil {
		t.Fatalf("handshake recv: %v", err)
	}

	if err := writeMsg(conn, Request{ID: "ai1", Method: "cli.ai", Prompt: "hello"}); err != nil {
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
	var got AIResult
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Reply != "hello from the agent" {
		t.Errorf("Reply = %q, want %q", got.Reply, "hello from the agent")
	}
	if got.SessionID != "sess-abc" {
		t.Errorf("SessionID = %q, want %q", got.SessionID, "sess-abc")
	}
	if got.TaskID != "buildin/dicodai" {
		t.Errorf("TaskID = %q, want %q", got.TaskID, "buildin/dicodai")
	}
}

// TestControl_AI_NumericSessionID verifies the fmt.Sprint-based relaxation
// in handleAI: alternative tasks that emit session_id as a JSON number (very
// common when ids are represented as u64 server-side) round-trip to the CLI
// as a string instead of being silently dropped — which would have meant a
// fresh session every turn.
func TestControl_AI_NumericSessionID(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	socketPath := filepath.Join(dir, "ctrl.sock")
	tokenPath := filepath.Join(dir, "ctrl.token")

	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()
	reg := registry.New(d)
	if err := reg.Register(&task.Spec{ID: "buildin/dicodai", Name: "dicodai"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	eng := &mockEngine{
		runID: "run-ai-num-1",
		result: RunResult{
			RunID:  "run-ai-num-1",
			Status: "success",
			ReturnValue: map[string]any{
				"session_id": float64(42), // JSON numbers decode to float64
				"reply":      "numeric session test",
			},
		},
	}

	cs, err := NewControlServer(socketPath, tokenPath, reg, eng, nil, MetricsProvider{}, "test", zap.NewNop(), nil, "buildin/dicodai")
	if err != nil {
		t.Fatalf("NewControlServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = cs.Start(ctx) }()

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

	tok, _ := os.ReadFile(tokenPath)
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = writeMsg(conn, handshakeReq{Token: string(tok)})
	var hs map[string]any
	_ = readMsg(conn, &hs)

	_ = writeMsg(conn, Request{ID: "ai1", Method: "cli.ai", Prompt: "hello"})
	var resp struct {
		ID     string          `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	_ = readMsg(conn, &resp)
	if resp.Error != "" {
		t.Fatalf("unexpected error: %s", resp.Error)
	}
	var got AIResult
	_ = json.Unmarshal(resp.Result, &got)
	if got.SessionID != "42" {
		t.Errorf("numeric session_id should stringify to %q, got %q", "42", got.SessionID)
	}
	if got.Reply != "numeric session test" {
		t.Errorf("Reply = %q, want %q", got.Reply, "numeric session test")
	}
}

// TestControl_AI_NoDefault_NoOverride_ReturnsError rejects the request when
// neither cfg.AI.Task nor req.TaskID is set — the daemon has nothing to fire.
func TestControl_AI_NoDefault_NoOverride_ReturnsError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	socketPath := filepath.Join(dir, "ctrl.sock")
	tokenPath := filepath.Join(dir, "ctrl.token")

	d, _ := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	defer d.Close()
	reg := registry.New(d)

	cs, err := NewControlServer(socketPath, tokenPath, reg, &mockEngine{}, nil, MetricsProvider{}, "test", zap.NewNop(), nil, "")
	if err != nil {
		t.Fatalf("NewControlServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = cs.Start(ctx) }()

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

	tok, _ := os.ReadFile(tokenPath)
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = writeMsg(conn, handshakeReq{Token: string(tok)})
	var hs map[string]any
	_ = readMsg(conn, &hs)

	_ = writeMsg(conn, Request{ID: "ai1", Method: "cli.ai", Prompt: "hello"})
	var resp struct {
		ID    string `json:"id"`
		Error string `json:"error"`
	}
	_ = readMsg(conn, &resp)
	if resp.Error == "" {
		t.Error("expected error when no ai task is configured")
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
