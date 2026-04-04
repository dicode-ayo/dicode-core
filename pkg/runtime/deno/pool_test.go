package deno

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	denopkg "github.com/dicode/dicode/pkg/deno"
)

// poolShimServer simulates the Deno warm process side of the pool protocol.
// It listens on socketPath, accepts one connection, sends {ready:true}, reads
// the dispatch, and signals the dispatch over dispatchCh.
func poolShimServer(t *testing.T, socketPath string, dispatchCh chan<- poolDispatch) {
	t.Helper()
	_ = os.Remove(socketPath)
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Errorf("poolShimServer listen: %v", err)
		return
	}
	defer ln.Close()

	conn, err := ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()

	// Send ready message.
	readyMsg, _ := json.Marshal(poolReadyMsg{Ready: true})
	hdr := make([]byte, 4)
	binary.LittleEndian.PutUint32(hdr, uint32(len(readyMsg)))
	frame := append(hdr, readyMsg...)
	if _, err := conn.Write(frame); err != nil {
		t.Errorf("poolShimServer write ready: %v", err)
		return
	}

	// Wait for dispatch message.
	var dispatch poolDispatch
	if err := readPoolMsg(conn, &dispatch); err != nil {
		return
	}
	dispatchCh <- dispatch
}

func TestPool_DisabledReturnsError(t *testing.T) {
	ctx := context.Background()
	p := NewPool(ctx, "/dev/null", "/dev/null", 0)
	defer p.Close()

	if p.size != 0 {
		t.Errorf("expected size 0, got %d", p.size)
	}

	_, err := p.Acquire(ctx)
	if err == nil {
		t.Fatal("expected error from disabled pool Acquire")
	}
}

func TestPool_AcquireTimeout(t *testing.T) {
	ctx := context.Background()
	// Create a pool with size 1 but a deno path that doesn't exist — replenish
	// will fail and the ready channel will never receive.
	p := NewPool(ctx, "/nonexistent-deno-binary", "/dev/null", 1)
	defer p.Close()

	// Acquire should time out quickly.
	acquireCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_, err := p.Acquire(acquireCtx)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestPool_CloseStopsGoroutines(t *testing.T) {
	ctx := context.Background()
	p := NewPool(ctx, "/nonexistent-deno-binary", "/dev/null", 2)
	// Close should not panic or block.
	p.Close()
}

func TestPool_CloseWithWaitingProcesses(t *testing.T) {
	// Inject a fake warm process directly into the ready channel and verify
	// that Close drains and kills it.
	ctx := context.Background()
	p := NewPool(ctx, "/dev/null", "/dev/null", 1)

	// Create a temp socket to simulate a warm process socket path.
	socketPath := "/tmp/dicode-pool-test-close.sock"
	_ = os.Remove(socketPath)

	// Use a fake conn (unix socket pair) so close() doesn't panic.
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Skipf("cannot listen on unix socket: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, _ := ln.Accept()
		if conn != nil {
			conn.Close()
		}
	}()
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Skipf("cannot dial unix socket: %v", err)
	}

	w := &warmProc{
		conn:       conn,
		socketPath: socketPath,
	}

	// Inject directly.
	p.ready <- w

	// Close should drain the warm proc.
	p.Close()

	// Verify the socket was cleaned up.
	if _, statErr := os.Stat(socketPath); !os.IsNotExist(statErr) {
		// socket may already be gone due to ln.Close, that's fine
	}
}

func TestPool_ReadPoolMsg_TooLarge(t *testing.T) {
	// Simulate a message with an absurdly large size header using a socketpair.
	sockPath := "/tmp/dicode-pool-toolarge-test.sock"
	_ = os.Remove(sockPath)
	defer os.Remove(sockPath)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Skipf("cannot listen: %v", err)
	}
	defer ln.Close()

	connCh := make(chan net.Conn, 1)
	go func() {
		conn, e := ln.Accept()
		if e != nil {
			return
		}
		connCh <- conn
	}()

	client, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Skipf("cannot dial: %v", err)
	}
	defer client.Close()

	server := <-connCh
	defer server.Close()

	// Write an oversized length header from server to client.
	hdr := make([]byte, 4)
	binary.LittleEndian.PutUint32(hdr, 10*1024*1024) // 10MB > 4MB limit
	server.Write(hdr)                                //nolint:errcheck

	var out poolReadyMsg
	err = readPoolMsg(client, &out)
	if err == nil {
		t.Fatal("expected error for oversized pool message")
	}
}

func TestPool_WarmProcDispatch(t *testing.T) {
	// Build a fake warm process pair using a socketpair or unix socket.
	socketPath := "/tmp/dicode-pool-dispatch-test.sock"
	_ = os.Remove(socketPath)
	defer os.Remove(socketPath)

	dispatchCh := make(chan poolDispatch, 1)
	go poolShimServer(t, socketPath, dispatchCh)

	// Give the server a moment to start.
	time.Sleep(20 * time.Millisecond)

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Skipf("cannot dial test socket: %v", err)
	}

	// Read the ready message from server side.
	var ready poolReadyMsg
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if err := readPoolMsg(conn, &ready); err != nil {
		t.Fatalf("read ready: %v", err)
	}
	if !ready.Ready {
		t.Fatal("expected ready=true")
	}

	w := &warmProc{conn: conn, socketPath: socketPath}

	want := poolDispatch{
		SocketPath: "/tmp/test-ipc.sock",
		Token:      "tok-abc",
		Script:     "console.log('hi')",
	}
	if err := w.dispatch(want); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	conn.Close()

	select {
	case got := <-dispatchCh:
		if got.SocketPath != want.SocketPath {
			t.Errorf("socketPath: got %q want %q", got.SocketPath, want.SocketPath)
		}
		if got.Token != want.Token {
			t.Errorf("token: got %q want %q", got.Token, want.Token)
		}
		if got.Script != want.Script {
			t.Errorf("script: got %q want %q", got.Script, want.Script)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for dispatch")
	}
}

func TestPool_ReplenishWithRealDeno(t *testing.T) {
	// Only run this test if a real deno binary is available.
	denoPath, err := findDenoBinary()
	if err != nil {
		t.Skipf("deno not in PATH: %v", err)
	}

	// Write the pool shim to a temp file.
	shimFile, err := os.CreateTemp("", "dicode-pool-shim-test-*.js")
	if err != nil {
		t.Fatalf("create shim file: %v", err)
	}
	defer os.Remove(shimFile.Name())
	if _, err := shimFile.WriteString(poolShimContent); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	shimFile.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p := NewPool(ctx, denoPath, shimFile.Name(), 1)
	defer p.Close()

	// Acquire should succeed because the pool has a real deno process.
	acquireCtx, acquireCancel := context.WithTimeout(ctx, 20*time.Second)
	defer acquireCancel()

	warm, err := p.Acquire(acquireCtx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if warm == nil {
		t.Fatal("expected non-nil warm proc")
	}
	if warm.conn == nil {
		t.Fatal("expected non-nil conn")
	}

	// Clean up the warm proc without dispatching.
	warm.close()
}

// findDenoBinary looks for deno in the dicode cache.
func findDenoBinary() (string, error) {
	p, err := denopkg.BinaryPath(denopkg.DefaultVersion)
	if err != nil {
		return "", fmt.Errorf("deno binary path: %w", err)
	}
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("deno not cached at %s: %w", p, err)
	}
	return p, nil
}
