package relay

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestHello_AdvertisesBothKeys drives Client.handshake against a minimal
// inspector server that captures the hello message. We assert that both
// `pubkey` (SignKey) and `decrypt_pubkey` (DecryptKey) are populated and
// match the daemon's keys — this is the wire-level guarantee of the
// #104 split on the daemon-to-broker side.
func TestHello_AdvertisesBothKeys(t *testing.T) {
	captured := make(chan helloMsg, 1)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			t.Errorf("ws accept: %v", err)
			return
		}
		defer conn.CloseNow()

		ctx := r.Context()

		// Send a minimal challenge so the client proceeds to send hello.
		nonce := make([]byte, 32)
		_, _ = rand.Read(nonce)
		ch, _ := encodeMsg(challengeMsg{Type: msgChallenge, Nonce: hex.EncodeToString(nonce)})
		if err := conn.Write(ctx, websocket.MessageText, ch); err != nil {
			return
		}

		// Read the hello and forward it to the test goroutine.
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var h helloMsg
		if err := json.Unmarshal(data, &h); err != nil {
			t.Errorf("parse hello: %v", err)
			return
		}
		select {
		case captured <- h:
		default:
		}

		// Send a welcome with protocol 2 so Client.SupportsOAuth would be true.
		w2, _ := encodeMsg(welcomeMsg{Type: msgWelcome, URL: "https://example.test/u/x/hooks/", Protocol: 2})
		_ = conn.Write(ctx, websocket.MessageText, w2)

		// Keep the connection up briefly so the client settles past handshake.
		time.Sleep(100 * time.Millisecond)
	}))
	defer ts.Close()

	id := NewTestIdentity(t)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")

	// A harmless local HTTP server so NewClient has a port to target.
	localSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer localSrv.Close()
	_, portStr, _ := net.SplitHostPort(localSrv.Listener.Addr().String())
	port, _ := strconv.Atoi(portStr)

	client := NewClient(wsURL, id, port, nil, noopLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = client.runOnce(ctx) }()

	select {
	case h := <-captured:
		// Verify pubkey matches SignPublicKey.
		wantSign := base64.StdEncoding.EncodeToString(id.SignPublicKey())
		if h.PubKey != wantSign {
			t.Fatalf("hello.pubkey does not match SignPublicKey\n got=%s\nwant=%s",
				h.PubKey, wantSign)
		}
		// Verify decrypt_pubkey matches DecryptPublicKey and is not empty.
		wantDecrypt := base64.StdEncoding.EncodeToString(id.DecryptPublicKey())
		if h.DecryptPubKey == "" {
			t.Fatal("hello.decrypt_pubkey is empty — new daemons must advertise it")
		}
		if h.DecryptPubKey != wantDecrypt {
			t.Fatalf("hello.decrypt_pubkey does not match DecryptPublicKey\n got=%s\nwant=%s",
				h.DecryptPubKey, wantDecrypt)
		}
		// And crucially, the two fields must be different values: if they
		// were equal the split would have been silently defeated (two
		// references to the same key).
		if h.PubKey == h.DecryptPubKey {
			t.Fatal("hello.pubkey == hello.decrypt_pubkey — SignKey and DecryptKey collapsed to one key")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for client hello")
	}
}
