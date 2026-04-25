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
	relaypb "github.com/dicode/dicode/pkg/relay/pb"
)

// TestClient_RefusesConnection_WhenBrokerProtocolOld covers the daemon-side
// half of the version gate: the client reads the broker's `protocol` field in
// the welcome message and refuses the connection outright when it is below
// BrokerProtocolMin. No silent fallback — the broker must be upgraded.
//
// Protocol 3 is the current floor (#195 protobuf-es migration). Earlier
// values represent older brokers that produced a different on-wire shape.
func TestClient_RefusesConnection_WhenBrokerProtocolOld(t *testing.T) {
	helper := func(proto *int32) *relaypb.Welcome {
		w := &relaypb.Welcome{Url: "https://x/u/x/hooks/"}
		if proto != nil {
			w.Protocol = proto
		}
		return w
	}
	two := int32(2)
	three := int32(3)

	cases := []struct {
		name          string
		welcome       *relaypb.Welcome
		wantHandshake bool // true = runOnce should complete the handshake successfully
	}{
		{"no protocol field", helper(nil), false},
		{"protocol 2", helper(&two), false},
		{"protocol 3", helper(&three), true},
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
				ch, _ := encodeServerMessage(&relaypb.ServerMessage{
					Kind: &relaypb.ServerMessage_Challenge{
						Challenge: &relaypb.Challenge{Nonce: hex.EncodeToString(nonce)},
					},
				})
				_ = conn.Write(ctx, websocket.MessageText, ch)

				_, _, _ = conn.Read(ctx)

				w2, _ := encodeServerMessage(&relaypb.ServerMessage{
					Kind: &relaypb.ServerMessage_Welcome{Welcome: welcome},
				})
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
				// Protocol >= BrokerProtocolMin — wait for the handshake to
				// complete, confirmed by hookBaseURL being set and
				// SupportsOAuth flipping to true.
				deadline := time.After(2 * time.Second)
				for {
					select {
					case <-deadline:
						t.Fatal("handshake did not settle within 2s")
					case <-time.After(25 * time.Millisecond):
						if client.HookBaseURL() != "" {
							if !client.SupportsOAuth() {
								t.Fatalf("protocol %d broker: SupportsOAuth should be true after handshake", welcome.GetProtocol())
							}
							cancel()
							<-errCh
							return
						}
					}
				}
			} else {
				// Protocol < BrokerProtocolMin — runOnce must return an error
				// containing the "upgrade dicode-relay" hint; SupportsOAuth
				// must stay false.
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
					t.Fatalf("SupportsOAuth must remain false when broker protocol < %d", BrokerProtocolMin)
				}
			}
		})
	}
}
