package relay

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"go.uber.org/zap"
)

// Server is a simple relay server. It accepts WebSocket connections from relay
// clients, authenticates them via ECDSA challenge-response, and forwards
// incoming HTTP webhook requests over the WebSocket.
//
// This implementation is intended for self-hosting and for integration tests.
// It keeps nonce state in memory; restarting the server invalidates used nonces
// (acceptable since nonce TTL is 60 s and the clock window is ±30 s).
type Server struct {
	mux            *http.ServeMux
	log            *zap.Logger
	host           string // e.g. "https://relay.example.com"
	originPatterns []string

	mu      sync.Mutex
	clients map[string]*serverConn // uuid → conn
	nonces  map[string]time.Time   // nonce hex → expiry
}

// NewServer creates a relay server. host is the public base URL used in
// welcome messages (e.g. "https://relay.example.com").
// originPatterns is the allowlist of Origin header patterns passed to
// websocket.AcceptOptions.OriginPatterns; pass nil or empty to disallow
// cross-origin connections entirely (most secure for a relay server).
func NewServer(host string, log *zap.Logger, originPatterns ...string) *Server {
	s := &Server{
		mux:            http.NewServeMux(),
		log:            log,
		host:           host,
		originPatterns: originPatterns,
		clients:        make(map[string]*serverConn),
		nonces:         make(map[string]time.Time),
	}
	s.mux.HandleFunc("/ws", s.handleWS)
	s.mux.HandleFunc("/u/", s.handleInbound)
	return s
}

// ServeHTTP implements http.Handler so the server can be embedded in tests.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// serverConn represents one authenticated client connection.
type serverConn struct {
	mu      sync.Mutex // serializes writes to conn
	conn    *websocket.Conn
	uuid    string
	pending sync.Map // request id → chan responseMsg
}

func (sc *serverConn) write(ctx context.Context, data []byte) error {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.conn.Write(ctx, websocket.MessageText, data)
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	opts := &websocket.AcceptOptions{}
	if len(s.originPatterns) > 0 {
		opts.OriginPatterns = s.originPatterns
	}
	conn, err := websocket.Accept(w, r, opts)
	if err != nil {
		s.log.Warn("relay: ws accept", zap.Error(err))
		return
	}
	defer conn.CloseNow()

	ctx := r.Context()

	nonce, err := s.sendChallenge(ctx, conn)
	if err != nil {
		s.log.Warn("relay: send challenge", zap.Error(err))
		return
	}

	sc, err := s.receiveHello(ctx, conn, nonce)
	if err != nil {
		s.log.Warn("relay: handshake failed", zap.Error(err))
		// Collapse all auth errors to a single opaque message (issue #7).
		errData, _ := encodeMsg(errorMsg{Type: msgError, Message: "authentication failed"})
		_ = conn.Write(ctx, websocket.MessageText, errData)
		return
	}

	welcomeURL := fmt.Sprintf("%s/u/%s/hooks/", s.host, sc.uuid)
	welcomeData, _ := encodeMsg(welcomeMsg{Type: msgWelcome, URL: welcomeURL, Protocol: 2})
	if err := sc.write(ctx, welcomeData); err != nil {
		return
	}

	s.mu.Lock()
	s.clients[sc.uuid] = sc
	s.mu.Unlock()

	s.log.Info("relay: client connected", zap.String("uuid", sc.uuid))

	defer func() {
		s.mu.Lock()
		delete(s.clients, sc.uuid)
		s.mu.Unlock()
		s.log.Info("relay: client disconnected", zap.String("uuid", sc.uuid))
	}()

	// Read responses from the client.
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if msgType(data) != msgResponse {
			continue
		}
		var resp responseMsg
		if err := json.Unmarshal(data, &resp); err != nil {
			continue
		}
		if ch, ok := sc.pending.Load(resp.ID); ok {
			ch.(chan responseMsg) <- resp
		}
	}
}

func (s *Server) sendChallenge(ctx context.Context, conn *websocket.Conn) ([]byte, error) {
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	nonceHex := hex.EncodeToString(nonce)

	data, err := encodeMsg(challengeMsg{Type: msgChallenge, Nonce: nonceHex})
	if err != nil {
		return nil, err
	}
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		return nil, err
	}

	// Only register the nonce after it has been successfully sent (issue #11).
	s.mu.Lock()
	s.nonces[nonceHex] = time.Now().Add(60 * time.Second)
	s.mu.Unlock()

	return nonce, nil
}

func (s *Server) receiveHello(ctx context.Context, conn *websocket.Conn, nonce []byte) (*serverConn, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	_, data, err := conn.Read(ctx)
	if err != nil {
		return nil, fmt.Errorf("read hello: %w", err)
	}
	if msgType(data) != msgHello {
		return nil, fmt.Errorf("expected hello, got %q", msgType(data))
	}

	var hello helloMsg
	if err := json.Unmarshal(data, &hello); err != nil {
		return nil, fmt.Errorf("parse hello: %w", err)
	}

	// Verify pubkey decodes properly.
	pubBytes, err := base64.StdEncoding.DecodeString(hello.PubKey)
	if err != nil {
		s.log.Debug("relay: decode pubkey failed", zap.Error(err))
		return nil, fmt.Errorf("authentication failed")
	}

	// Parse and validate the public key (replaces deprecated elliptic.Unmarshal).
	pub, err := unmarshalUncompressed(pubBytes)
	if err != nil {
		s.log.Debug("relay: invalid public key", zap.Error(err))
		return nil, fmt.Errorf("authentication failed")
	}

	// Verify UUID matches pubkey using constant-time comparison (issue #4).
	sum := sha256.Sum256(pubBytes)
	computedUUID := hex.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(computedUUID), []byte(hello.UUID)) != 1 {
		s.log.Debug("relay: uuid mismatch", zap.String("claimed", hello.UUID), zap.String("computed", computedUUID))
		return nil, fmt.Errorf("authentication failed")
	}

	// Verify timestamp window.
	now := time.Now().Unix()
	if hello.Timestamp < now-30 || hello.Timestamp > now+30 {
		s.log.Debug("relay: timestamp out of window", zap.Int64("timestamp", hello.Timestamp))
		return nil, fmt.Errorf("authentication failed")
	}

	// Verify nonce is not replayed.
	nonceHex := hex.EncodeToString(nonce)
	s.mu.Lock()
	expiry, known := s.nonces[nonceHex]
	if known {
		delete(s.nonces, nonceHex)
	}
	s.mu.Unlock()

	if !known || time.Now().After(expiry) {
		s.log.Debug("relay: nonce expired or unknown")
		return nil, fmt.Errorf("authentication failed")
	}

	// Verify signature.
	sigBytes, err := base64.StdEncoding.DecodeString(hello.Sig)
	if err != nil {
		s.log.Debug("relay: decode sig failed", zap.Error(err))
		return nil, fmt.Errorf("authentication failed")
	}

	var tsBuf [8]byte
	binary.BigEndian.PutUint64(tsBuf[:], uint64(hello.Timestamp))
	h := sha256.New()
	h.Write(nonce)
	h.Write(tsBuf[:])
	digest := h.Sum(nil)

	if !ecdsa.VerifyASN1(pub, digest, sigBytes) {
		s.log.Debug("relay: invalid signature")
		return nil, fmt.Errorf("authentication failed")
	}

	return &serverConn{conn: conn, uuid: hello.UUID}, nil
}

// handleInbound handles incoming webhook HTTP requests and forwards them to
// the connected relay client.
//
// URL pattern: /u/<uuid>/hooks/<path>
func (s *Server) handleInbound(w http.ResponseWriter, r *http.Request) {
	// Extract uuid from path using safe string operations (issue #12).
	path := r.URL.Path // e.g. /u/abc123/hooks/my-task
	rest, ok := strings.CutPrefix(path, "/u/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	uuid, hookPath, ok := strings.Cut(rest, "/")
	if !ok || uuid == "" {
		http.NotFound(w, r)
		return
	}
	hookPath = "/" + hookPath

	s.mu.Lock()
	sc, ok := s.clients[uuid]
	s.mu.Unlock()

	if !ok {
		http.Error(w, "relay client not connected", http.StatusServiceUnavailable)
		return
	}

	// Read body with a size limit to prevent memory exhaustion (issue #2).
	limited := io.LimitReader(r.Body, maxBodySize+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if int64(len(body)) > maxBodySize {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	reqID := newRequestID()
	headers := make(map[string][]string)
	for k, vals := range r.Header {
		headers[k] = vals
	}

	reqMsg := requestMsg{
		Type:    msgRequest,
		ID:      reqID,
		Method:  r.Method,
		Path:    hookPath,
		Headers: headers,
		Body:    base64.StdEncoding.EncodeToString(body),
	}

	respCh := make(chan responseMsg, 1)
	sc.pending.Store(reqID, respCh)
	defer sc.pending.Delete(reqID)

	data, err := encodeMsg(reqMsg)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := sc.write(ctx, data); err != nil {
		http.Error(w, "relay write error", http.StatusBadGateway)
		return
	}

	select {
	case <-ctx.Done():
		http.Error(w, "relay timeout", http.StatusGatewayTimeout)
		return
	case resp := <-respCh:
		for k, vals := range resp.Headers {
			if hopByHopHeaders[k] {
				continue
			}
			for _, v := range vals {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.Status)
		if resp.Body != "" {
			decoded, err := base64.StdEncoding.DecodeString(resp.Body)
			if err == nil {
				_, _ = w.Write(decoded)
			}
		}
	}
}

func newRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
