package relay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
)

// Tests in this file address issue #122: WSS reconnect + broker TOFU pin
// + key-rotation verification. Scenarios 1 (ECDSA challenge-response) and
// 6 (timestamp / nonce replay protection) are already covered by
// client_test.go and challenge_sig_test.go; these tests cover the TOFU
// lifecycle gaps (2, 3, 4, 5 from the issue acceptance).
//
// Design note: instead of hand-rolling a stub broker, these tests drive
// the real in-repo Server (pkg/relay.Server) and use the new
// SetBrokerPubkey hook to simulate key rotation across reconnects. That
// keeps the production wire protocol as the single source of truth —
// no risk of a divergent stub implementation covering up a real drift.

// generateBrokerPubkeyB64 returns a fresh ECDSA P-256 public key encoded
// as base64 SPKI DER — the same shape dicode-relay advertises in its
// welcome messages.
func generateBrokerPubkeyB64(t *testing.T) string {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate broker key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal SPKI: %v", err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

// setupTOFURelay spins up a relay Server advertising the given broker pubkey
// and returns the httptest wrapper + the ws:// URL the client should dial.
func setupTOFURelay(t *testing.T, brokerPubB64 string) (*httptest.Server, string, *Server) {
	t.Helper()
	log := noopLogger()
	srv := NewServer("https://example.com", log)
	srv.SetBrokerPubkey(brokerPubB64)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	return ts, wsURL, srv
}

// waitForPubkey polls c.BrokerPubkey() until it returns want or deadline
// elapses. Fails the test if the poll times out.
func waitForPubkey(t *testing.T, c *Client, want string, deadline time.Duration, msg string) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if c.BrokerPubkey() == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s: BrokerPubkey() = %q, want %q after %s", msg, c.BrokerPubkey(), want, deadline)
}

// loopbackPort extracts the port number from an httptest.Server listener
// address. Unlike a bare string split this handles IPv6 literals correctly.
func loopbackPort(t *testing.T, addr string) int {
	t.Helper()
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split host/port %q: %v", addr, err)
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port %q: %v", portStr, err)
	}
	return p
}

// localLoopback runs a trivial no-op HTTP server on a random local port and
// returns the port. For tests that just need the daemon's relay.Client to
// have a forwarding target but don't care about request handling.
func localLoopback(t *testing.T) int {
	t.Helper()
	ls := httptest.NewServer(nil)
	t.Cleanup(ls.Close)
	return loopbackPort(t, ls.Listener.Addr().String())
}

// ---------------------------------------------------------------------------
// Scenario 2: broker pubkey is pinned on first connection
// ---------------------------------------------------------------------------

func TestTOFU_PinOnFirstConnect(t *testing.T) {
	brokerPub := generateBrokerPubkeyB64(t)
	_, wsURL, _ := setupTOFURelay(t, brokerPub)
	database := openTestDB(t)

	// Pre-assertion: no pin.
	got, err := LoadBrokerPubkey(context.Background(), database)
	if err != nil {
		t.Fatalf("LoadBrokerPubkey before connect: %v", err)
	}
	if got != "" {
		t.Fatalf("pre-connect pin = %q, want empty", got)
	}

	client := NewClient(wsURL, newTestIdentity(t), localLoopback(t), database, noopLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- client.Run(ctx) }()

	waitForPubkey(t, client, brokerPub, 5*time.Second,
		"first connect should pin the broker pubkey")

	// And the DB-side pin matches.
	got, err = LoadBrokerPubkey(context.Background(), database)
	if err != nil {
		t.Fatalf("LoadBrokerPubkey after connect: %v", err)
	}
	if got != brokerPub {
		t.Fatalf("stored pin = %q, want %q", got, brokerPub)
	}
}

// ---------------------------------------------------------------------------
// Scenario 3: reconnect after network drop succeeds without re-pinning
// ---------------------------------------------------------------------------

// TestTOFU_PinPreservedAcrossReconnects proves two things that have to hold
// together: (a) the daemon successfully re-handshakes after a dropped
// connection, and (b) the TOFU pin is not re-written on that successful
// reconnect. Proving a reconnect *happened* matters — the client's
// in-memory BrokerPubkey and the DB pin both survive disconnect on their
// own, so asserting on their values alone is a trivial passing test.
// Following the `TestAutoReconnect` pattern we ping through the relay's
// public webhook URL; the local daemon-side handler signals a fresh hit,
// confirming the full WS tunnel is live end-to-end on each iteration.
func TestTOFU_PinPreservedAcrossReconnects(t *testing.T) {
	brokerPub := generateBrokerPubkeyB64(t)
	ts, wsURL, srv := setupTOFURelay(t, brokerPub)
	database := openTestDB(t)

	// Local daemon-side HTTP server: signals on every /hooks/ping hit so
	// the test can observe when a fresh WS tunnel actually carries
	// traffic end-to-end (not just when a welcome was received).
	pinged := make(chan struct{}, 10)
	localSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hooks/ping" {
			select {
			case pinged <- struct{}{}:
			default:
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(localSrv.Close)
	port := loopbackPort(t, localSrv.Listener.Addr().String())

	id := newTestIdentity(t)
	client := NewClient(wsURL, id, port, database, noopLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	waitForPubkey(t, client, brokerPub, 5*time.Second, "initial connect")

	// pingUntilDelivered polls /hooks/ping through the public relay URL
	// until the daemon-local handler signals receipt, confirming a live
	// WS tunnel. Fails the test on deadline.
	pingUntilDelivered := func(label string) {
		t.Helper()
		hookURL := fmt.Sprintf("%s/u/%s/hooks/ping", ts.URL, id.UUID)
		deadline := time.After(5 * time.Second)
		for {
			select {
			case <-deadline:
				t.Fatalf("%s: webhook never reached daemon via relay", label)
			case <-pinged:
				return
			default:
			}
			resp, err := http.Post(hookURL, "application/json", strings.NewReader("{}"))
			if resp != nil {
				resp.Body.Close()
			}
			_ = err
			time.Sleep(100 * time.Millisecond)
		}
	}

	// First iteration: prove the tunnel is live.
	pingUntilDelivered("pre-drop")

	// Drain any stragglers from the first ping before we measure the
	// second — `pingUntilDelivered` loops until it sees at least one
	// signal, but extra retries may have queued up.
	for {
		select {
		case <-pinged:
			continue
		default:
		}
		break
	}

	// Drop the tunnel. httptest.Server.CloseClientConnections is a no-op
	// on hijacked WS connections; severAllWS reaches into the server's
	// client registry and closes each active ws.Conn directly, forcing
	// the daemon's read loop to return and runOnce to exit. Run's
	// backoff-and-retry cycle then lands a fresh handshake.
	severAllWS(srv)

	// Second iteration: prove the tunnel comes back. The fact that a
	// NEW ping propagates (after the old connection was severed) is the
	// cross-component signal that a reconnect actually happened.
	pingUntilDelivered("post-drop — reconnect must have completed")

	// Invariant under test: the TOFU pin must have stayed put across
	// the reconnect. CheckAndPinBrokerPubkey should have returned
	// BrokerPubkeyPinMatch (not PinNew) — a stored value of anything
	// other than brokerPub would mean the DB was re-written on the
	// re-handshake, which breaks the whole TOFU guarantee.
	stored, err := LoadBrokerPubkey(context.Background(), database)
	if err != nil {
		t.Fatalf("load pin after reconnect: %v", err)
	}
	if stored != brokerPub {
		t.Fatalf("pin changed across reconnect: got %q, want %q", stored, brokerPub)
	}
	if client.BrokerPubkey() != brokerPub {
		t.Fatalf("client.BrokerPubkey() = %q after reconnect, want %q",
			client.BrokerPubkey(), brokerPub)
	}
}

// ---------------------------------------------------------------------------
// Scenarios 4 & 5 use the sequential-daemons pattern: pin under broker K1,
// then start a fresh daemon against broker K2, sharing the same DB. This
// avoids waiting for the real client's exponential-backoff reconnect loop
// to catch up after a mid-connection rotation — we already have #137's
// testcontainers suite for the "live reconnect under load" flavor; here
// we isolate the TOFU state-machine transitions and drive them directly.
// ---------------------------------------------------------------------------

func startDaemonAndWait(t *testing.T, wsURL string, database db.DB, wantPub string, stable time.Duration) (*Client, context.CancelFunc) {
	t.Helper()
	client := NewClient(wsURL, newTestIdentity(t), localLoopback(t), database, noopLogger())
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = client.Run(ctx) }()
	if wantPub != "" {
		waitForPubkey(t, client, wantPub, stable, "handshake completion")
	}
	return client, cancel
}

// ---------------------------------------------------------------------------
// Scenario 4: reconnect fails hard when the broker presents a different key
// ---------------------------------------------------------------------------

func TestTOFU_RefusesAfterBrokerKeyRotation(t *testing.T) {
	origPub := generateBrokerPubkeyB64(t)
	newPub := generateBrokerPubkeyB64(t)
	if origPub == newPub {
		t.Fatal("freshly generated keys should differ; test assumption violated")
	}
	_, wsURL, srv := setupTOFURelay(t, origPub)
	database := openTestDB(t)

	// Daemon 1 pins origPub.
	_, cancel1 := startDaemonAndWait(t, wsURL, database, origPub, 5*time.Second)
	cancel1()

	// Broker rotates. Daemon 2 reconnects and must NOT accept the new
	// welcome.broker_pubkey: the stored pin is still origPub, so
	// CheckAndPinBrokerPubkey returns Mismatch and the handshake aborts.
	srv.SetBrokerPubkey(newPub)

	client2, cancel2 := startDaemonAndWait(t, wsURL, database, "", 0)
	defer cancel2()

	// Give the client a handful of handshake cycles. Each attempt hits
	// the mismatch branch — none should update state. The pin must stay
	// origPub on disk and the in-memory cache must not report newPub.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := client2.BrokerPubkey(); got == newPub {
			t.Fatalf("client accepted rotated broker pubkey: BrokerPubkey() = %q; "+
				"mismatch path must abort the handshake before updating the cache", got)
		}
		time.Sleep(100 * time.Millisecond)
	}

	stored, err := LoadBrokerPubkey(context.Background(), database)
	if err != nil {
		t.Fatalf("load pin: %v", err)
	}
	if stored != origPub {
		t.Fatalf("DB pin overwritten on mismatch: got %q, want %q; "+
			"CheckAndPinBrokerPubkey must NOT overwrite on mismatch — that would "+
			"silently accept a rotated broker key and defeat TOFU", stored, origPub)
	}
}

// ---------------------------------------------------------------------------
// Scenario 5: `dicode relay trust-broker --yes` clears the pin and the next
// TOFU succeeds against the new key.
// ---------------------------------------------------------------------------

func TestTOFU_TrustBrokerClearsPinAndRepins(t *testing.T) {
	origPub := generateBrokerPubkeyB64(t)
	newPub := generateBrokerPubkeyB64(t)
	_, wsURL, srv := setupTOFURelay(t, origPub)
	database := openTestDB(t)

	// Daemon 1 pins origPub.
	_, cancel1 := startDaemonAndWait(t, wsURL, database, origPub, 5*time.Second)
	cancel1()

	// Broker rotates to newPub. Daemon 2 would fail with mismatch (tested
	// separately in TestTOFU_RefusesAfterBrokerKeyRotation), so skip it
	// here — go directly to the operator intervention.
	srv.SetBrokerPubkey(newPub)

	// Operator runs `dicode relay trust-broker --yes`. The CLI path
	// resolves to ReplaceBrokerPubkey.
	if err := ReplaceBrokerPubkey(context.Background(), database, newPub); err != nil {
		t.Fatalf("ReplaceBrokerPubkey (simulated trust-broker CLI): %v", err)
	}

	// Daemon 2 starts fresh. Its handshake should now see a matching pin
	// and complete successfully.
	_, cancel2 := startDaemonAndWait(t, wsURL, database, newPub, 5*time.Second)
	defer cancel2()

	stored, err := LoadBrokerPubkey(context.Background(), database)
	if err != nil {
		t.Fatalf("load pin: %v", err)
	}
	if stored != newPub {
		t.Fatalf("post-trust-broker pin = %q, want %q", stored, newPub)
	}
}
