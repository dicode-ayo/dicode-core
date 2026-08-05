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
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// Tests for the cli.ready readiness barrier (#464). Handler-level tests
// exercise the wait/timeout/shutdown paths in isolation; the socket-level
// regression test reproduces the CI race: a task lookup issued after the
// control socket is up but before the first reconcile registers the task.

func TestControl_Ready_NilSignalAlwaysReady(t *testing.T) {
	t.Parallel()
	cs := &ControlServer{log: zap.NewNop()} // no SetReadySignal — tests / stripped builds
	res, err := cs.handleReady(context.Background(), Request{})
	if err != nil {
		t.Fatalf("handleReady: %v", err)
	}
	if !res.Ready {
		t.Fatal("nil ready signal must report ready")
	}
}

func TestControl_Ready_ProbeReturnsImmediately(t *testing.T) {
	t.Parallel()
	cs := &ControlServer{log: zap.NewNop()}
	cs.SetReadySignal(make(chan struct{})) // never closed

	// WaitMs 0 is a pure probe — must not block.
	res, err := cs.handleReady(context.Background(), Request{})
	if err != nil {
		t.Fatalf("handleReady: %v", err)
	}
	if res.Ready {
		t.Fatal("unclosed ready signal must report not-ready")
	}
}

func TestControl_Ready_TimesOutCleanly(t *testing.T) {
	t.Parallel()
	cs := &ControlServer{log: zap.NewNop()}
	cs.SetReadySignal(make(chan struct{})) // never closed

	start := time.Now()
	res, err := cs.handleReady(context.Background(), Request{WaitMs: 50})
	if err != nil {
		t.Fatalf("handleReady: %v", err)
	}
	if res.Ready {
		t.Fatal("timed-out wait must report not-ready")
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("wait returned after %v, want >= 50ms", elapsed)
	}
}

func TestControl_Ready_UnblocksOnSignal(t *testing.T) {
	t.Parallel()
	cs := &ControlServer{log: zap.NewNop()}
	ready := make(chan struct{})
	cs.SetReadySignal(ready)

	done := make(chan ReadyResult, 1)
	go func() {
		res, _ := cs.handleReady(context.Background(), Request{WaitMs: int((10 * time.Second).Milliseconds())})
		done <- res
	}()
	close(ready)

	select {
	case res := <-done:
		if !res.Ready {
			t.Fatal("wait must report ready once the signal closes")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handleReady did not unblock on the ready signal")
	}
}

// TestControl_Ready_UnblocksOnShutdown pins the shutdown-safety contract: a
// wait against a first sync that never completes must not outlive the
// connection/daemon context — no goroutine may be stranded selecting on a
// channel that never closes.
func TestControl_Ready_UnblocksOnShutdown(t *testing.T) {
	t.Parallel()
	cs := &ControlServer{log: zap.NewNop()}
	cs.SetReadySignal(make(chan struct{})) // never closed — sync never finishes

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := cs.handleReady(ctx, Request{WaitMs: int((10 * time.Second).Milliseconds())})
		done <- err
	}()
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("shutdown during wait must surface ctx error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handleReady did not unblock on ctx cancellation")
	}
}

// readyRegressionEnv is controlTestEnv with a real registry and a
// caller-controlled ready signal, so a test can drive the socket-up-but-not-
// reconciled startup window.
func readyRegressionEnv(t *testing.T, ready <-chan struct{}) (net.Conn, *registry.Registry, func()) {
	t.Helper()

	dir := t.TempDir()
	socketPath := filepath.Join(dir, "ctrl.sock")
	tokenPath := filepath.Join(dir, "ctrl.token")

	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	reg := registry.New(d)

	cs, err := NewControlServer(socketPath, tokenPath, reg, &mockEngine{}, nil, MetricsProvider{}, "test", zap.NewNop(), nil, "", "")
	if err != nil {
		t.Fatalf("NewControlServer: %v", err)
	}
	cs.SetReadySignal(ready)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = cs.Start(ctx)
	}()

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

	tok, err := readCLITokenFile(tokenPath)
	if err != nil {
		cancel()
		t.Fatalf("read token: %v", err)
	}
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		cancel()
		t.Fatalf("dial control: %v", err)
	}
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
	return conn, reg, cleanup
}

// TestControl_Ready_RegressionTaskLookupAfterFirstSync reproduces issue #464
// end-to-end over the socket: the control socket accepts connections before
// the first reconcile has registered any task, so an immediate lookup fails
// with `task "X" not found`. A client that gates on cli.ready instead blocks
// until the first sync lands and then finds the task.
func TestControl_Ready_RegressionTaskLookupAfterFirstSync(t *testing.T) {
	t.Parallel()

	ready := make(chan struct{})
	conn, reg, cleanup := readyRegressionEnv(t, ready)
	defer cleanup()

	// Socket is up, daemon not ready: the pre-#464 failure mode. A direct
	// task lookup sees "not found" for a task about to be registered.
	if err := writeMsg(conn, Request{ID: "r1", Method: "cli.task.test", TaskID: "buildin/webui"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	var resp Response
	if err := readMsg(conn, &resp); err != nil {
		t.Fatalf("recv: %v", err)
	}
	if !strings.Contains(resp.Error, `task "buildin/webui" not found`) {
		t.Fatalf("pre-sync lookup error = %q, want task-not-found", resp.Error)
	}

	// Simulate the first reconcile completing while a cli.ready wait is
	// pending: register the task, then close the ready signal.
	taskDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(taskDir, "task.yaml"), []byte("name: webui\ntrigger:\n  manual: true\nruntime: deno\n"), 0644)
	_ = os.WriteFile(filepath.Join(taskDir, "task.ts"), []byte("export default () => ({})\n"), 0644)
	spec := &task.Spec{
		ID: "buildin/webui", Name: "webui", Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true}, Timeout: 5 * time.Second, TaskDir: taskDir,
	}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("register: %v", err)
	}
	close(ready)

	// The bounded wait now reports ready...
	if err := writeMsg(conn, Request{ID: "r2", Method: "cli.ready", WaitMs: int((10 * time.Second).Milliseconds())}); err != nil {
		t.Fatalf("send: %v", err)
	}
	resp = Response{}
	if err := readMsg(conn, &resp); err != nil {
		t.Fatalf("recv: %v", err)
	}
	if resp.Error != "" {
		t.Fatalf("cli.ready error: %s", resp.Error)
	}
	rm, ok := resp.Result.(map[string]any)
	if !ok || rm["ready"] != true {
		t.Fatalf("cli.ready result = %#v, want ready:true", resp.Result)
	}

	// ...and the same lookup no longer reports not-found. (The registered
	// task has no test file, so the handler fails later with a different,
	// registration-independent error.)
	if err := writeMsg(conn, Request{ID: "r3", Method: "cli.task.test", TaskID: "buildin/webui"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	resp = Response{}
	if err := readMsg(conn, &resp); err != nil {
		t.Fatalf("recv: %v", err)
	}
	if strings.Contains(resp.Error, "not found") {
		t.Fatalf("post-sync lookup still not-found: %q", resp.Error)
	}
	if !strings.Contains(resp.Error, "no test file") {
		t.Fatalf("post-sync lookup error = %q, want no-test-file (proves registry hit)", resp.Error)
	}
}
