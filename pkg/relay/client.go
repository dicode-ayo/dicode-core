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
	"math"
	mathrand "math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/coder/websocket"
	"go.uber.org/zap"
)

// Client maintains a persistent WebSocket connection to a relay server and
// forwards incoming webhook requests through a local http.Handler.
type Client struct {
	serverURL string
	identity  *Identity
	handler   http.Handler
	log       *zap.Logger
}

// NewClient creates a relay client. serverURL must be a ws:// or wss:// URL.
func NewClient(serverURL string, identity *Identity, handler http.Handler, log *zap.Logger) *Client {
	return &Client{
		serverURL: serverURL,
		identity:  identity,
		handler:   handler,
		log:       log,
	}
}

// Run connects to the relay server and maintains the connection until ctx is
// cancelled. Reconnects with exponential backoff on disconnect.
func (c *Client) Run(ctx context.Context) error {
	backoff := time.Second
	const maxBackoff = 60 * time.Second

	for {
		if err := c.runOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			c.log.Warn("relay disconnected, reconnecting", zap.Error(err), zap.Duration("backoff", backoff))
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(jitter(backoff)):
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
	conn, _, err := websocket.Dial(ctx, c.serverURL, nil)
	if err != nil {
		return fmt.Errorf("dial relay: %w", err)
	}
	defer conn.CloseNow()

	if err := c.handshake(ctx, conn); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}

	return c.serve(ctx, conn)
}

func (c *Client) handshake(ctx context.Context, conn *websocket.Conn) error {
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

	if err := conn.Write(ctx, websocket.MessageText, hello); err != nil {
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

func (c *Client) serve(ctx context.Context, conn *websocket.Conn) error {
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

		go c.handleRequest(ctx, conn, req)
	}
}

func (c *Client) handleRequest(ctx context.Context, conn *websocket.Conn, req requestMsg) {
	resp := c.dispatchRequest(req)
	out, err := encodeMsg(resp)
	if err != nil {
		c.log.Error("relay: encode response", zap.Error(err))
		return
	}
	if err := conn.Write(ctx, websocket.MessageText, out); err != nil {
		c.log.Warn("relay: send response", zap.Error(err))
	}
}

func (c *Client) dispatchRequest(req requestMsg) responseMsg {
	var body []byte
	if req.Body != "" {
		var err error
		body, err = base64.StdEncoding.DecodeString(req.Body)
		if err != nil {
			return errorResponse(req.ID, http.StatusBadRequest)
		}
	}

	httpReq, err := http.NewRequest(req.Method, req.Path, bytes.NewReader(body))
	if err != nil {
		return errorResponse(req.ID, http.StatusBadRequest)
	}
	if !strings.HasPrefix(req.Path, "http") {
		httpReq.RequestURI = req.Path
	}
	for k, vals := range req.Headers {
		for _, v := range vals {
			httpReq.Header.Add(k, v)
		}
	}

	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, httpReq)

	result := rec.Result()
	var respBody []byte
	if result.Body != nil {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(result.Body)
		_ = result.Body.Close()
		respBody = buf.Bytes()
	}

	headers := make(map[string][]string)
	for k, vals := range result.Header {
		headers[k] = vals
	}

	return responseMsg{
		Type:    msgResponse,
		ID:      req.ID,
		Status:  result.StatusCode,
		Headers: headers,
		Body:    base64.StdEncoding.EncodeToString(respBody),
	}
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
