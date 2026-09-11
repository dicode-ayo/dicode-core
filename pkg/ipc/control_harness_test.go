package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"testing"
	"time"
)

// Shared scaffolding for the control-socket tests: start a server, wait for
// its socket, connect and handshake. Every control test needs all three, and
// nothing about them is specific to what is being tested.

// serveControl starts cs and blocks until its socket file exists, returning
// the function that shuts it down and waits for Start to return.
func serveControl(t *testing.T, cs *ControlServer, socketPath string) func() {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = cs.Start(ctx)
	}()
	stop := func() {
		cancel()
		<-done
	}
	if err := waitForSocket(socketPath, 2*time.Second); err != nil {
		stop()
		t.Fatal(err)
	}
	return stop
}

// waitForSocket polls for the socket file, which Start creates from a
// goroutine. Start offers no readiness signal, so a deadline poll is the only
// handle a test has.
func waitForSocket(socketPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(socketPath); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("control socket %s never appeared within %s", socketPath, timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// dialControl connects to the control socket and completes the handshake with
// the token the server wrote, returning a conn ready for request/response
// exchanges.
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
