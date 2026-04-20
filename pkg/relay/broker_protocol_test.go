package relay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestClient_RefusesOAuth_WhenBrokerProtocolOld covers the daemon-side half of
// the version gate from decision 3 in the #104 review: the client reads the
// broker's `protocol` field in the welcome message and only reports
// SupportsOAuth when it is >= 2.
//
// The test drives three variants of the welcome message through a minimal
// test broker:
//   - no protocol field at all (legacy pre-#104 broker)
//   - protocol: 1 (explicit pre-split)
//   - protocol: 2 (post-split)
//
// SupportsOAuth must be false for the first two and true for the last.
func TestClient_RefusesOAuth_WhenBrokerProtocolOld(t *testing.T) {
	cases := []struct {
		name    string
		welcome welcomeMsg
		want    bool
	}{
		{"no protocol field", welcomeMsg{Type: msgWelcome, URL: "https://x/u/x/hooks/"}, false},
		{"protocol 1", welcomeMsg{Type: msgWelcome, URL: "https://x/u/x/hooks/", Protocol: 1}, false},
		{"protocol 2", welcomeMsg{Type: msgWelcome, URL: "https://x/u/x/hooks/", Protocol: 2}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			welcome := tc.welcome

			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
					InsecureSkipVerify: true,
				})
				if err != nil {
					return
				}
				defer conn.CloseNow()

				ctx := r.Context()

				// Send a dummy challenge. We don't verify the hello; we
				// only care that the client processes our welcome.
				nonce := make([]byte, 32)
				_, _ = rand.Read(nonce)
				ch, _ := encodeMsg(challengeMsg{Type: msgChallenge, Nonce: hex.EncodeToString(nonce)})
				_ = conn.Write(ctx, websocket.MessageText, ch)

				// Drain the hello message so the client is ready for welcome.
				_, _, _ = conn.Read(ctx)

				w2, _ := encodeMsg(welcome)
				_ = conn.Write(ctx, websocket.MessageText, w2)

				// Keep the connection alive briefly so Client.serve doesn't
				// race us to Read before we've returned.
				time.Sleep(150 * time.Millisecond)
			}))
			defer ts.Close()

			id := NewTestIdentity(t)
			wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")

			localSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			defer localSrv.Close()
			_, portStr, _ := net.SplitHostPort(localSrv.Listener.Addr().String())
			port, _ := strconv.Atoi(portStr)

			client := NewClient(wsURL, id, port, nil, noopLogger())

			// Precondition: SupportsOAuth must be false before any handshake.
			if client.SupportsOAuth() {
				t.Fatal("SupportsOAuth should be false before handshake")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			done := make(chan struct{})
			go func() {
				_ = client.runOnce(ctx)
				close(done)
			}()

			// Wait for the welcome to be processed (poll SupportsOAuth).
			deadline := time.After(2 * time.Second)
			var settled bool
			for !settled {
				select {
				case <-deadline:
					t.Fatal("handshake did not settle within 2s")
				case <-time.After(25 * time.Millisecond):
					// Cheap way to detect "the welcome has been applied":
					// once client.hookBaseURL is non-empty it means
					// Client.handshake returned, which means brokerProtocol
					// has been recorded.
					if client.HookBaseURL() != "" {
						settled = true
					}
				}
			}

			if got := client.SupportsOAuth(); got != tc.want {
				t.Fatalf("SupportsOAuth = %v, want %v (protocol=%d)",
					got, tc.want, welcome.Protocol)
			}

			cancel()
			<-done
		})
	}
}
