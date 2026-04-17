// Package relay implements a WebSocket relay client that lets a local dicode
// instance receive webhooks from external services without port forwarding.
package relay

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	mathrand "math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/dicode/dicode/pkg/db"
	"go.uber.org/zap"
)

const (
	// maxBodySize is the maximum request body size accepted by the relay (5 MB).
	maxBodySize = 5 * 1024 * 1024

	// dialTimeout is the maximum time allowed for the WebSocket dial + handshake.
	dialTimeout = 15 * time.Second

	// stableConnectionThreshold is the minimum time a connection must stay up
	// before the backoff resets on the next reconnect.
	stableConnectionThreshold = 10 * time.Second
)

// Client maintains a persistent WebSocket connection to a relay server and
// forwards incoming webhook requests to the local daemon HTTP server.
type Client struct {
	serverURL   string
	identity    *Identity
	localPort   int
	db          db.DB // for TOFU broker pubkey pinning
	log         *zap.Logger
	localClient *http.Client

	hookMu      sync.RWMutex
	hookBaseURL string // set after successful handshake from welcome message

	brokerMu     sync.RWMutex
	brokerPubkey string // cached pinned broker pubkey (base64 SPKI DER)
}

// BrokerPubkey returns the currently pinned broker public key (base64 SPKI DER).
// Returns "" if the broker hasn't announced one yet.
func (c *Client) BrokerPubkey() string {
	c.brokerMu.RLock()
	defer c.brokerMu.RUnlock()
	return c.brokerPubkey
}

// NewClient creates a relay client. serverURL must be a wss:// URL.
// In test/dev environments ws:// is accepted but a warning is logged.
func NewClient(serverURL string, identity *Identity, localPort int, database db.DB, log *zap.Logger) *Client {
	if !strings.HasPrefix(serverURL, "wss://") && !strings.HasPrefix(serverURL, "ws://") {
		log.Error("relay: serverURL must start with wss:// or ws://", zap.String("url", serverURL))
	} else if strings.HasPrefix(serverURL, "ws://") {
		log.Warn("relay: using unencrypted ws:// connection — use wss:// in production",
			zap.String("url", serverURL))
	}
	return &Client{
		serverURL:   serverURL,
		identity:    identity,
		localPort:   localPort,
		db:          database,
		log:         log,
		localClient: &http.Client{Timeout: 25 * time.Second},
	}
}

// HookBaseURL returns the relay hook base URL received from the server after a
// successful handshake (e.g. "https://relay.dicode.app/u/<uuid>/hooks/").
// Returns empty string if not yet connected.
func (c *Client) HookBaseURL() string {
	c.hookMu.RLock()
	defer c.hookMu.RUnlock()
	return c.hookBaseURL
}

// Run connects to the relay server and maintains the connection until ctx is
// cancelled. Reconnects with exponential backoff on disconnect.
func (c *Client) Run(ctx context.Context) error {
	backoff := time.Second
	const maxBackoff = 60 * time.Second

	for {
		connectedAt := time.Now()
		if err := c.runOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			c.log.Warn("relay disconnected, reconnecting", zap.Error(err), zap.Duration("backoff", backoff))
		}

		// Reset backoff if the connection was stable long enough.
		if time.Since(connectedAt) >= stableConnectionThreshold {
			backoff = time.Second
		}

		t := time.NewTimer(jitter(backoff))
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}

		backoff = time.Duration(math.Min(float64(backoff*2), float64(maxBackoff)))
	}
}

// jitter returns d ±20%.
func jitter(d time.Duration) time.Duration {
	f := float64(d)
	delta := f * 0.2
	offset := (mathrand.Float64()*2 - 1) * delta
	return time.Duration(f + offset)
}

func (c *Client) runOnce(ctx context.Context) error {
	c.hookMu.Lock()
	c.hookBaseURL = ""
	c.hookMu.Unlock()

	// Apply a timeout to the dial + handshake phase.
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	conn, _, err := websocket.Dial(dialCtx, c.serverURL, nil)
	if err != nil {
		return fmt.Errorf("dial relay: %w", err)
	}
	defer conn.CloseNow()

	// sendMu serializes all writes to this connection (required by coder/websocket).
	var sendMu sync.Mutex

	if err := c.handshake(dialCtx, conn, &sendMu); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	cancel() // handshake done — release the dial timeout

	return c.serve(ctx, conn, &sendMu)
}

func (c *Client) handshake(ctx context.Context, conn *websocket.Conn, sendMu *sync.Mutex) error {
	_, data, err := conn.Read(ctx)
	if err != nil {
		return fmt.Errorf("read challenge: %w", err)
	}
	if msgType(data) != msgChallenge {
		return fmt.Errorf("expected challenge, got %q", msgType(data))
	}
	var ch challengeMsg
	if err := json.Unmarshal(data, &ch); err != nil {
		return fmt.Errorf("parse challenge: %w", err)
	}

	nonceBytes, err := hex.DecodeString(ch.Nonce)
	if err != nil || len(nonceBytes) != 32 {
		return fmt.Errorf("invalid nonce")
	}

	ts := time.Now().Unix()
	sig, err := signChallenge(c.identity.PrivateKey, nonceBytes, ts)
	if err != nil {
		return fmt.Errorf("sign challenge: %w", err)
	}

	pubKeyB64 := base64.StdEncoding.EncodeToString(c.identity.UncompressedPublicKey())

	hello, err := encodeMsg(helloMsg{
		Type:      msgHello,
		UUID:      c.identity.UUID,
		PubKey:    pubKeyB64,
		Sig:       base64.StdEncoding.EncodeToString(sig),
		Timestamp: ts,
	})
	if err != nil {
		return fmt.Errorf("encode hello: %w", err)
	}

	sendMu.Lock()
	err = conn.Write(ctx, websocket.MessageText, hello)
	sendMu.Unlock()
	if err != nil {
		return fmt.Errorf("send hello: %w", err)
	}

	_, data, err = conn.Read(ctx)
	if err != nil {
		return fmt.Errorf("read welcome: %w", err)
	}

	switch msgType(data) {
	case msgWelcome:
		var w welcomeMsg
		if err := json.Unmarshal(data, &w); err != nil {
			return fmt.Errorf("parse welcome: %w", err)
		}
		c.hookMu.Lock()
		c.hookBaseURL = w.URL
		c.hookMu.Unlock()

		// TOFU broker pubkey pinning: on first connect, store the broker's
		// signing pubkey. On reconnect, verify it hasn't changed.
		if w.BrokerPubkey != "" && c.db != nil {
			result, err := CheckAndPinBrokerPubkey(ctx, c.db, w.BrokerPubkey)
			if err != nil {
				return fmt.Errorf("broker pubkey pin: %w", err)
			}
			switch result {
			case BrokerPubkeyPinNew:
				c.log.Info("relay: pinned broker signing key (trust-on-first-use)",
					zap.String("pubkey", w.BrokerPubkey[:16]+"…"))
			case BrokerPubkeyPinMatch:
				// Expected path on reconnect — nothing to log.
			case BrokerPubkeyPinMismatch:
				return fmt.Errorf(
					"relay: BROKER PUBKEY CHANGED — the relay server presented a different signing key " +
						"than the one pinned on first connect. If the relay operator rotated their key, " +
						"run `dicode relay trust-broker --yes` to accept the new key. " +
						"Connection rejected to prevent token substitution attacks")
			}
			c.brokerMu.Lock()
			c.brokerPubkey = w.BrokerPubkey
			c.brokerMu.Unlock()
		}

		c.log.Info("relay connected", zap.String("url", w.URL))
		return nil
	case msgError:
		var e errorMsg
		_ = json.Unmarshal(data, &e)
		return fmt.Errorf("relay rejected handshake: %s", e.Message)
	default:
		return fmt.Errorf("unexpected message type %q after hello", msgType(data))
	}
}

func (c *Client) serve(ctx context.Context, conn *websocket.Conn, sendMu *sync.Mutex) error {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("read: %w", err)
		}

		if msgType(data) != msgRequest {
			c.log.Warn("relay: unexpected message type", zap.String("type", msgType(data)))
			continue
		}

		var req requestMsg
		if err := json.Unmarshal(data, &req); err != nil {
			c.log.Warn("relay: parse request", zap.Error(err))
			continue
		}

		go c.handleRequest(ctx, conn, sendMu, req)
	}
}

func (c *Client) handleRequest(ctx context.Context, conn *websocket.Conn, sendMu *sync.Mutex, req requestMsg) {
	resp := c.dispatchRequest(req)
	out, err := encodeMsg(resp)
	if err != nil {
		c.log.Error("relay: encode response", zap.Error(err))
		return
	}
	sendMu.Lock()
	err = conn.Write(ctx, websocket.MessageText, out)
	sendMu.Unlock()
	if err != nil {
		c.log.Warn("relay: send response", zap.Error(err))
	}
}

func (c *Client) dispatchRequest(req requestMsg) responseMsg {
	var body []byte
	if req.Body != "" {
		// Limit base64-decoded body to maxBodySize to avoid memory exhaustion.
		// The base64 string itself can be at most ~4/3 * maxBodySize.
		maxB64 := int64(maxBodySize * 4 / 3)
		limited := io.LimitReader(strings.NewReader(req.Body), maxB64+1)
		b64Data, err := io.ReadAll(limited)
		if err != nil || int64(len(b64Data)) > maxB64 {
			return errorResponse(req.ID, http.StatusRequestEntityTooLarge)
		}
		body, err = base64.StdEncoding.DecodeString(string(b64Data))
		if err != nil {
			return errorResponse(req.ID, http.StatusBadRequest)
		}
		if len(body) > maxBodySize {
			return errorResponse(req.ID, http.StatusRequestEntityTooLarge)
		}
	}

	// Only forward requests to /hooks/ and /dicode.js paths — reject anything
	// else to limit blast radius if the relay server is compromised.
	if !strings.HasPrefix(req.Path, "/hooks/") && req.Path != "/dicode.js" {
		return errorResponse(req.ID, http.StatusForbidden)
	}

	targetURL := fmt.Sprintf("http://localhost:%d%s", c.localPort, req.Path)
	httpReq, err := http.NewRequestWithContext(context.Background(), req.Method, targetURL, bytes.NewReader(body))
	if err != nil {
		return errorResponse(req.ID, http.StatusBadRequest)
	}
	for k, vals := range req.Headers {
		if http.CanonicalHeaderKey(k) == "X-Relay-Base" {
			continue
		}
		for _, v := range vals {
			httpReq.Header.Add(k, v)
		}
	}

	// Set X-Relay-Base using the client's known UUID — no URL parsing needed.
	httpReq.Header.Set("X-Relay-Base", "/u/"+c.identity.UUID)

	resp, err := c.localClient.Do(httpReq)
	if err != nil {
		c.log.Warn("relay: local request failed", zap.Error(err))
		return errorResponse(req.ID, http.StatusBadGateway)
	}
	defer resp.Body.Close()

	var respBody []byte
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(io.LimitReader(resp.Body, maxBodySize))
	respBody = buf.Bytes()

	headers := filterResponseHeaders(resp.Header)

	return responseMsg{
		Type:    msgResponse,
		ID:      req.ID,
		Status:  resp.StatusCode,
		Headers: headers,
		Body:    base64.StdEncoding.EncodeToString(respBody),
	}
}

// hopByHopHeaders are headers that must not be forwarded per HTTP/1.1.
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailers":            true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	// Sensitive headers that should not leak to the inbound caller.
	"Set-Cookie": true,
}

// filterResponseHeaders strips hop-by-hop and sensitive headers before
// forwarding the response back to the inbound webhook caller.
func filterResponseHeaders(h http.Header) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, vals := range h {
		if hopByHopHeaders[k] {
			continue
		}
		out[k] = vals
	}
	return out
}

func errorResponse(id string, status int) responseMsg {
	return responseMsg{
		Type:    msgResponse,
		ID:      id,
		Status:  status,
		Headers: map[string][]string{},
		Body:    "",
	}
}

// signChallenge signs sha256(nonce || timestamp_big_endian_uint64) with key.
func signChallenge(key *ecdsa.PrivateKey, nonce []byte, ts int64) ([]byte, error) {
	var tsBuf [8]byte
	binary.BigEndian.PutUint64(tsBuf[:], uint64(ts))

	h := sha256.New()
	h.Write(nonce)
	h.Write(tsBuf[:])
	digest := h.Sum(nil)

	return ecdsa.SignASN1(rand.Reader, key, digest)
}
