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

// TestClient_RefusesConnection_WhenBrokerProtocolOld covers the daemon-side
// half of the version gate from decision 3 in the #104 review: the client
// reads the broker's `protocol` field in the welcome message and refuses the
// connection outright when it is below 2. No silent fallback — the broker must
// be upgraded to honour the split sign/decrypt key model.
//
// Three welcome variants:
//   - no protocol field at all (legacy pre-#104 broker) → connection refused
//   - protocol: 1 (explicit pre-split)                  → connection refused
//   - protocol: 2 (post-split)                          → connection accepted
func TestClient_RefusesConnection_WhenBrokerProtocolOld(t *testing.T) {
	cases := []struct {
		name          string
		welcome       welcomeMsg
		wantHandshake bool // true = runOnce should complete the handshake successfully
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

				nonce := make([]byte, 32)
				_, _ = rand.Read(nonce)
				ch, _ := encodeMsg(challengeMsg{Type: msgChallenge, Nonce: hex.EncodeToString(nonce)})
				_ = conn.Write(ctx, websocket.MessageText, ch)

				_, _, _ = conn.Read(ctx)

				w2, _ := encodeMsg(welcome)
				_ = conn.Write(ctx, websocket.MessageText, w2)

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

			if client.SupportsOAuth() {
				t.Fatal("SupportsOAuth should be false before handshake")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			errCh := make(chan error, 1)
			go func() {
				errCh <- client.runOnce(ctx)
			}()

			if tc.wantHandshake {
				// protocol 2 — wait for the handshake to complete, confirmed by
				// the hookBaseURL being set and SupportsOAuth flipping to true.
				deadline := time.After(2 * time.Second)
				for {
					select {
					case <-deadline:
						t.Fatal("handshake did not settle within 2s")
					case <-time.After(25 * time.Millisecond):
						if client.HookBaseURL() != "" {
							if !client.SupportsOAuth() {
								t.Fatalf("protocol 2 broker: SupportsOAuth should be true after handshake")
							}
							cancel()
							<-errCh
							return
						}
					}
				}
			} else {
				// protocol < 2 — runOnce must return an error containing the
				// "upgrade dicode-relay" hint; SupportsOAuth must stay false.
				select {
				case err := <-errCh:
					if err == nil {
						t.Fatalf("expected connection refusal error, got nil")
					}
					if !strings.Contains(err.Error(), "upgrade dicode-relay") {
						t.Fatalf("error should mention upgrade dicode-relay: got %v", err)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("runOnce did not return within 2s")
				}
				if client.SupportsOAuth() {
					t.Fatalf("SupportsOAuth must remain false when broker protocol < 2")
				}
			}
		})
	}
}
