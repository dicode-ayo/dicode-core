package ipc

import (
	"fmt"
	"net"
	"time"
)

// ControlClient is a synchronous client for the daemon's control socket.
// It connects, performs the handshake, and exposes a Send method for CLI
// command dispatch. Each CLI invocation creates one client, sends one
// request, and closes.
type ControlClient struct {
	conn net.Conn
	caps []string

	// socketPath/tokenPath let SendFresh open an independent connection for a
	// request that must not wait behind one already in flight on conn.
	socketPath string
	tokenPath  string
}

// Dial connects to the daemon control socket at socketPath and authenticates.
// On Linux, authentication happens via SO_PEERCRED (UID match) — tokenPath is
// ignored and may not exist on disk. On other platforms, the token is read
// from tokenPath and sent in the handshake.
func Dial(socketPath, tokenPath string) (*ControlClient, error) {
	tok, err := readCLITokenFile(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("read token file %s: %w", tokenPath, err)
	}

	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect to daemon at %s: %w", socketPath, err)
	}

	c := &ControlClient{conn: conn, socketPath: socketPath, tokenPath: tokenPath}
	if err := c.handshake(tok); err != nil {
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

// SendFresh performs req on a brand-new connection, independent of any
// request in flight on c's own connection. Send is half-duplex on a single
// synchronous connection (one fixed request ID, one reader), so a caller that
// needs to issue a request while c is blocked in Send elsewhere (e.g.
// cancelling a run mid-cli.run.wait) must use SendFresh rather than Send.
func (c *ControlClient) SendFresh(req Request) (Response, error) {
	fresh, err := Dial(c.socketPath, c.tokenPath)
	if err != nil {
		return Response{}, err
	}
	defer fresh.Close()
	return fresh.Send(req)
}

// Close closes the underlying connection.
func (c *ControlClient) Close() error { return c.conn.Close() }

// Caps returns the capability list granted by the daemon during handshake.
func (c *ControlClient) Caps() []string { return c.caps }
