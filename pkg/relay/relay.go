// Package relay manages the persistent WebSocket tunnel to dicode.app.
// It gives local dicode instances a stable public webhook URL without
// requiring port forwarding or a VPS.
//
// Architecture:
//
//	GitHub → POST dicode.app/u/{uid}/hooks/{path}
//	           → dicode.app relay server
//	               → WebSocket tunnel (this package)
//	                   → local dicode webhook handler
//
// The tunnel reconnects automatically with exponential backoff.
// When no account token is configured, the relay is disabled silently.
package relay

import "context"

// Client manages the WebSocket tunnel to dicode.app.
type Client struct {
	token   string // dicode.app account token
	baseURL string // relay server base URL (default: wss://relay.dicode.app)
}

// New creates a relay client. token is the dicode.app account token.
// If token is empty, all methods are no-ops.
func New(token, baseURL string) *Client {
	if baseURL == "" {
		baseURL = "wss://relay.dicode.app"
	}
	return &Client{token: token, baseURL: baseURL}
}

// Start connects to the relay and forwards incoming webhook payloads to
// handler. Reconnects automatically. Blocks until ctx is cancelled.
func (c *Client) Start(ctx context.Context, handler WebhookHandler) error {
	if c.token == "" {
		// No account configured — relay disabled, return immediately.
		<-ctx.Done()
		return nil
	}
	// TODO: implement WebSocket connection + reconnect loop
	<-ctx.Done()
	return nil
}

// WebhookURL returns the public webhook URL prefix for this account.
// Returns empty string if no token is configured.
func (c *Client) WebhookURL() string {
	if c.token == "" {
		return ""
	}
	// TODO: derive from token claims or fetch from relay API
	return ""
}

// WebhookHandler is called when a forwarded webhook arrives from the relay.
type WebhookHandler func(path string, headers map[string]string, body []byte)
