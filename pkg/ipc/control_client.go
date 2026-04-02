package ipc

import (
	"fmt"
	"net"
	"os"
	"time"
)

// ControlClient is a synchronous client for the daemon's control socket.
// It connects, performs the handshake, and exposes a Send method for CLI
// command dispatch. Each CLI invocation creates one client, sends one
// request, and closes.
type ControlClient struct {
	conn net.Conn
	caps []string
}

// Dial connects to the daemon control socket at socketPath and authenticates
// using the token stored in tokenPath. Returns a ready-to-use ControlClient.
func Dial(socketPath, tokenPath string) (*ControlClient, error) {
	tok, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("read token file %s: %w", tokenPath, err)
	}

	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect to daemon at %s: %w", socketPath, err)
	}

	c := &ControlClient{conn: conn}
	if err := c.handshake(string(tok)); err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

func (c *ControlClient) handshake(token string) error {
	_ = c.conn.SetDeadline(time.Now().Add(5 * time.Second))
	defer func() { _ = c.conn.SetDeadline(time.Time{}) }()

	if err := writeMsg(c.conn, handshakeReq{Token: token}); err != nil {
		return fmt.Errorf("handshake send: %w", err)
	}

	// Decode into a struct that covers both the success and error envelopes.
	// The server sends either {"proto":1,"caps":[...]} or {"error":"..."}.
	var raw struct {
		Proto int      `json:"proto"`
		Caps  []string `json:"caps"`
		Error string   `json:"error"`
	}
	if err := readMsg(c.conn, &raw); err != nil {
		return fmt.Errorf("handshake recv: %w", err)
	}
	if raw.Error != "" {
		return fmt.Errorf("handshake rejected: %s", raw.Error)
	}
	if raw.Proto == 0 {
		return fmt.Errorf("handshake: unexpected response from daemon (proto=0)")
	}
	c.caps = raw.Caps
	return nil
}

// Send sends a single request to the daemon and returns the response.
// The request ID is set automatically.
func (c *ControlClient) Send(req Request) (Response, error) {
	req.ID = "1"
	if err := writeMsg(c.conn, req); err != nil {
		return Response{}, fmt.Errorf("send: %w", err)
	}
	var resp Response
	if err := readMsg(c.conn, &resp); err != nil {
		return Response{}, fmt.Errorf("recv: %w", err)
	}
	return resp, nil
}

// Close closes the underlying connection.
func (c *ControlClient) Close() error { return c.conn.Close() }

// Caps returns the capability list granted by the daemon during handshake.
func (c *ControlClient) Caps() []string { return c.caps }
