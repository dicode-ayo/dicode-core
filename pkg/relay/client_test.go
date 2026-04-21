package relay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"go.uber.org/zap"
)

// newTestServer starts an in-process relay server and returns its URL and a
// cleanup function.
func newTestServer(t *testing.T) (wsURL string, httpURL string) {
	t.Helper()
	log := noopLogger()
	srv := NewServer("", log) // host left empty; tests check welcome differently

	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	httpURL = ts.URL
	wsURL = "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	return wsURL, httpURL
}

func newTestIdentity(t *testing.T) *Identity {
	t.Helper()
	ctx := context.Background()
	database := openTestDB(t)
	id, err := LoadOrGenerateIdentity(ctx, database, zap.NewNop())
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	return id
}

func TestHandshakeSuccess(t *testing.T) {
	wsURL, _ := newTestServer(t)
	id := newTestIdentity(t)
	log := noopLogger()

	localSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer localSrv.Close()
	_, portStr, _ := net.SplitHostPort(localSrv.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)

	client := NewClient(wsURL, id, port, nil, log)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// runOnce should complete the handshake and then block on serve().
	// We cancel the context to terminate it.
	done := make(chan error, 1)
	go func() { done <- client.runOnce(ctx) }()

	// Give it time to connect and handshake.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		// nil or context cancelled — both acceptable after a successful handshake.
		if err != nil && !isContextError(err) {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for client to finish")
	}
}

func TestHandshakeWrongKey(t *testing.T) {
	wsURL, _ := newTestServer(t)

	// Create identity with a valid key...
	id1 := newTestIdentity(t)
	// ...then create a different identity and use its key but id1's UUID.
	id2 := newTestIdentity(t)
	tamperedID := &Identity{
		SignKey:    id2.SignKey,    // wrong sign key
		DecryptKey: id2.DecryptKey, // any valid decrypt key; irrelevant for handshake
		UUID:       id1.UUID,       // mismatched UUID
	}

	log := noopLogger()
	localSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer localSrv.Close()
	_, portStr, _ := net.SplitHostPort(localSrv.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)
	client := NewClient(wsURL, tamperedID, port, nil, log)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := client.runOnce(ctx)
	if err == nil {
		t.Fatal("expected handshake to fail with mismatched UUID/key, got nil")
	}
	if !strings.Contains(err.Error(), "handshake") {
		t.Fatalf("expected handshake error, got: %v", err)
	}
}

func TestHandshakeReplayedTimestamp(t *testing.T) {
	// This test directly exercises the server's receiveHello with a stale timestamp.
	log := noopLogger()
	srv := NewServer("https://example.com", log)

	ts := httptest.NewServer(srv)
	defer ts.Close()

	id := newTestIdentity(t)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	// We'll intercept by modifying the timestamp in a custom client.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := dialWS(ctx, wsURL)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	// Read challenge.
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read challenge: %v", err)
	}
	var ch challengeMsg
	if err := json.Unmarshal(data, &ch); err != nil {
		t.Fatalf("parse challenge: %v", err)
	}

	nonceBytes, err := hex.DecodeString(ch.Nonce)
	if err != nil {
		t.Fatalf("decode nonce: %v", err)
	}

	// Use a timestamp 60 seconds in the past (outside ±30 s window).
	staleTS := time.Now().Unix() - 60
	sig, err := signChallenge(id.SignKey, nonceBytes, staleTS)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	hello, _ := encodeMsg(helloMsg{
		Type:      msgHello,
		UUID:      id.UUID,
		PubKey:    base64.StdEncoding.EncodeToString(id.SignPublicKey()),
		Sig:       base64.StdEncoding.EncodeToString(sig),
		Timestamp: staleTS,
	})
	if err := conn.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatalf("send hello: %v", err)
	}

	// Expect error response.
	_, data, err = conn.Read(ctx)
	if err != nil {
		// Connection may be closed — that's also a rejection.
		return
	}
	if msgType(data) != msgError {
		t.Fatalf("expected error message, got %q: %s", msgType(data), data)
	}
}

func TestWebhookForwarding(t *testing.T) {
	log := noopLogger()
	srv := NewServer("https://example.com", log)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	id := newTestIdentity(t)

	received := make(chan string, 1)
	localSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"ok":true}`)
	}))
	defer localSrv.Close()
	_, portStr, _ := net.SplitHostPort(localSrv.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)

	client := NewClient(wsURL, id, port, nil, log)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go func() { _ = client.Run(ctx) }()

	// Wait for the client to connect.
	time.Sleep(300 * time.Millisecond)

	// Send an inbound HTTP request to the relay server.
	hookPath := fmt.Sprintf("%s/u/%s/hooks/test-task", ts.URL, id.UUID)
	resp, err := http.Post(hookPath, "application/json", strings.NewReader(`{"event":"push"}`))
	if err != nil {
		t.Fatalf("post webhook: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	select {
	case path := <-received:
		if path != "/hooks/test-task" {
			t.Fatalf("handler got path %q, want /hooks/test-task", path)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout: handler not called")
	}
}

func TestAutoReconnect(t *testing.T) {
	log := noopLogger()

	// connectedCh is closed each time a client completes the welcome handshake.
	// We use a buffered channel big enough for multiple reconnects.
	connectedCh := make(chan struct{}, 10)

	// Wrap the relay server so we can detect each successful connection by
	// intercepting the inbound webhook path. Instead we use a dedicated test
	// handler that fires connectedCh whenever the relay server registers a new
	// client. We achieve this by using the /u/<uuid>/hooks/ping path: the
	// client's handler sends a signal on the channel.
	id := newTestIdentity(t)

	// Relay server.
	srv := NewServer("https://example.com", log)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	localSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hooks/ping" {
			select {
			case connectedCh <- struct{}{}:
			default:
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer localSrv.Close()
	_, portStr, _ := net.SplitHostPort(localSrv.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)

	client := NewClient(wsURL, id, port, nil, log)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	go func() { _ = client.Run(ctx) }()

	// Helper: poll until a webhook reaches the handler (proves connection is live).
	pingUntilConnected := func(label string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case <-deadline:
				t.Fatalf("timeout: %s — webhook never reached handler", label)
			default:
			}
			hookURL := fmt.Sprintf("%s/u/%s/hooks/ping", ts.URL, id.UUID)
			resp, err := http.Post(hookURL, "application/json", strings.NewReader("{}"))
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					// Drain the channel if the signal arrived.
					select {
					case <-connectedCh:
					default:
					}
					return
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	// Wait for the first connection to be live.
	pingUntilConnected("first connection")

	// Drop all active connections.
	ts.CloseClientConnections()

	// Give the client a moment to detect the drop and start reconnecting.
	time.Sleep(200 * time.Millisecond)

	// Verify the client reconnects and the webhook reaches the handler again.
	pingUntilConnected("reconnect after drop")
}

func TestRelayBaseHeader(t *testing.T) {
	log := noopLogger()
	srv := NewServer("https://relay.example.com", log)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	id := newTestIdentity(t)

	receivedHeader := make(chan string, 1)
	localSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeader <- r.Header.Get("X-Relay-Base")
		w.WriteHeader(http.StatusOK)
	}))
	defer localSrv.Close()
	_, portStr, _ := net.SplitHostPort(localSrv.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)

	client := NewClient(wsURL, id, port, nil, log)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go func() { _ = client.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	hookPath := fmt.Sprintf("%s/u/%s/hooks/test", ts.URL, id.UUID)
	resp, err := http.Post(hookPath, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	select {
	case hdr := <-receivedHeader:
		expected := "/u/" + id.UUID
		if hdr != expected {
			t.Fatalf("X-Relay-Base = %q, want %q", hdr, expected)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout")
	}
}

func isContextError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "context") || strings.Contains(s, "canceled") ||
		strings.Contains(s, "closed") || strings.Contains(s, "EOF")
}

// ---------------------------------------------------------------------------
// Security tests (issue #16)
// ---------------------------------------------------------------------------

// TestNonceReplay verifies that a nonce cannot be used twice.
func TestNonceReplay(t *testing.T) {
	log := noopLogger()
	srv := NewServer("https://example.com", log)
	ts := httptest.NewServer(srv)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	id := newTestIdentity(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// --- First attempt: valid handshake, grab the nonce. ---
	conn1, _, err := dialWS(ctx, wsURL)
	if err != nil {
		t.Fatalf("dial1: %v", err)
	}
	_, data, err := conn1.Read(ctx)
	if err != nil {
		t.Fatalf("read challenge1: %v", err)
	}
	var ch1 challengeMsg
	if err := json.Unmarshal(data, &ch1); err != nil {
		t.Fatalf("parse challenge1: %v", err)
	}
	nonce1, _ := hex.DecodeString(ch1.Nonce)

	ts1 := time.Now().Unix()
	sig1, _ := signChallenge(id.SignKey, nonce1, ts1)
	hello1, _ := encodeMsg(helloMsg{
		Type:      msgHello,
		UUID:      id.UUID,
		PubKey:    base64.StdEncoding.EncodeToString(id.SignPublicKey()),
		Sig:       base64.StdEncoding.EncodeToString(sig1),
		Timestamp: ts1,
	})
	if err := conn1.Write(ctx, websocket.MessageText, hello1); err != nil {
		t.Fatalf("send hello1: %v", err)
	}
	// Read welcome (first connection succeeds).
	_, resp1, err := conn1.Read(ctx)
	if err != nil {
		t.Fatalf("read welcome1: %v", err)
	}
	if msgType(resp1) != msgWelcome {
		t.Fatalf("expected welcome on first attempt, got %q: %s", msgType(resp1), resp1)
	}
	conn1.CloseNow()

	// --- Second attempt: replay nonce1 on a fresh connection. ---
	conn2, _, err := dialWS(ctx, wsURL)
	if err != nil {
		t.Fatalf("dial2: %v", err)
	}
	defer conn2.CloseNow()

	// Drain the new challenge (we won't use it).
	_, _, err = conn2.Read(ctx)
	if err != nil {
		t.Fatalf("read challenge2: %v", err)
	}

	// Re-use nonce1 (which was already consumed).
	ts2 := time.Now().Unix()
	sig2, _ := signChallenge(id.SignKey, nonce1, ts2)
	hello2, _ := encodeMsg(helloMsg{
		Type:      msgHello,
		UUID:      id.UUID,
		PubKey:    base64.StdEncoding.EncodeToString(id.SignPublicKey()),
		Sig:       base64.StdEncoding.EncodeToString(sig2),
		Timestamp: ts2,
	})
	if err := conn2.Write(ctx, websocket.MessageText, hello2); err != nil {
		t.Fatalf("send hello2: %v", err)
	}

	// The server must reject this (nonce unknown / already deleted).
	_, resp2, err := conn2.Read(ctx)
	if err != nil {
		return // connection closed — also a rejection
	}
	if msgType(resp2) != msgError {
		t.Fatalf("expected auth error on nonce replay, got %q: %s", msgType(resp2), resp2)
	}
	var errMsg errorMsg
	_ = json.Unmarshal(resp2, &errMsg)
	if errMsg.Message != "authentication failed" {
		t.Fatalf("expected opaque auth error, got %q", errMsg.Message)
	}
}

// TestMalformedPubKey verifies that a truncated / invalid public key is rejected.
func TestMalformedPubKey(t *testing.T) {
	log := noopLogger()
	srv := NewServer("https://example.com", log)
	ts := httptest.NewServer(srv)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cases := []struct {
		name   string
		pubKey []byte
	}{
		{"too short", make([]byte, 32)},
		{"wrong prefix", append([]byte{0x03}, make([]byte, 64)...)},
		{"not on curve", append([]byte{0x04}, make([]byte, 64)...)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, _, err := dialWS(ctx, wsURL)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.CloseNow()

			_, data, err := conn.Read(ctx)
			if err != nil {
				t.Fatalf("read challenge: %v", err)
			}
			var ch challengeMsg
			if err := json.Unmarshal(data, &ch); err != nil {
				t.Fatalf("parse challenge: %v", err)
			}
			nonceBytes, _ := hex.DecodeString(ch.Nonce)

			// Compute a fake UUID from the bad pubkey bytes.
			sum := sha256.Sum256(tc.pubKey)
			fakeUUID := hex.EncodeToString(sum[:])

			ts := time.Now().Unix()
			// Sign with a real key — doesn't matter, server should reject before sig check.
			realKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			sig, _ := signChallenge(realKey, nonceBytes, ts)

			hello, _ := encodeMsg(helloMsg{
				Type:      msgHello,
				UUID:      fakeUUID,
				PubKey:    base64.StdEncoding.EncodeToString(tc.pubKey),
				Sig:       base64.StdEncoding.EncodeToString(sig),
				Timestamp: ts,
			})
			if err := conn.Write(ctx, websocket.MessageText, hello); err != nil {
				t.Fatalf("send hello: %v", err)
			}

			_, resp, err := conn.Read(ctx)
			if err != nil {
				return // connection closed — rejection
			}
			if msgType(resp) != msgError {
				t.Fatalf("expected error, got %q: %s", msgType(resp), resp)
			}
			var errMsg errorMsg
			_ = json.Unmarshal(resp, &errMsg)
			if errMsg.Message != "authentication failed" {
				t.Fatalf("expected opaque auth error, got %q", errMsg.Message)
			}
		})
	}
}

// TestWrongUUID verifies that claiming someone else's UUID is rejected.
func TestWrongUUID(t *testing.T) {
	log := noopLogger()
	srv := NewServer("https://example.com", log)
	ts := httptest.NewServer(srv)
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	id := newTestIdentity(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, _, err := dialWS(ctx, wsURL)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read challenge: %v", err)
	}
	var ch challengeMsg
	if err := json.Unmarshal(data, &ch); err != nil {
		t.Fatalf("parse challenge: %v", err)
	}
	nonceBytes, _ := hex.DecodeString(ch.Nonce)

	ts2 := time.Now().Unix()
	sig, _ := signChallenge(id.SignKey, nonceBytes, ts2)

	// Send a valid pubkey but a wrong UUID (all-zeros).
	hello, _ := encodeMsg(helloMsg{
		Type:      msgHello,
		UUID:      strings.Repeat("0", 64),
		PubKey:    base64.StdEncoding.EncodeToString(id.SignPublicKey()),
		Sig:       base64.StdEncoding.EncodeToString(sig),
		Timestamp: ts2,
	})
	if err := conn.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatalf("send hello: %v", err)
	}

	_, resp, err := conn.Read(ctx)
	if err != nil {
		return // connection closed — rejection
	}
	if msgType(resp) != msgError {
		t.Fatalf("expected error for wrong UUID, got %q: %s", msgType(resp), resp)
	}
	var errMsg errorMsg
	_ = json.Unmarshal(resp, &errMsg)
	if errMsg.Message != "authentication failed" {
		t.Fatalf("expected opaque auth error, got %q", errMsg.Message)
	}
}
