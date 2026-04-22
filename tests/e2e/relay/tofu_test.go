//go:build e2e

package relay_e2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/relay"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

// Tests in this file address dicode-core#122: WSS reconnect + broker TOFU pin
// + key-rotation verification — against the real dicode-relay Node image
// (not an in-repo Go stub). The same scenarios were previously covered by
// an in-process Go `Server` stub in pkg/relay/reconnect_tofu_test.go;
// that file + the whole pkg/relay.Server are removed in this PR because
// the stub was the Go side testing itself and couldn't catch cross-impl
// drift. Full-container tests cost a few extra seconds per run but the
// signal is the actual wire contract.

// -----------------------------------------------------------------------------
// TOFU-specific helpers
// -----------------------------------------------------------------------------

// openTOFUDB opens an in-memory SQLite + ensures the kv table exists. The
// schema inline-create is a small duplicate of the daemon-side migration
// but pkg/relay's test helper isn't reachable from this package; the
// shape is small and stable.
func openTOFUDB(t *testing.T) db.DB {
	t.Helper()
	database, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := database.Exec(context.Background(),
		`CREATE TABLE IF NOT EXISTS kv (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	); err != nil {
		database.Close() //nolint:errcheck
		t.Fatalf("create kv table: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

// startDaemonAgainst wires a real relay.Client to h.wsBase, using the
// given db (for TOFU pin persistence across daemon lifecycles) and
// identity. If waitForWelcome is true, blocks until the handshake
// completes (HookBaseURL populated). Returns the client + a cancel
// to stop the Run loop.
func startDaemonAgainst(
	t *testing.T,
	h *relayHandle,
	database db.DB,
	identity *relay.Identity,
	waitForWelcome bool,
) (*relay.Client, context.CancelFunc) {
	t.Helper()

	// Any-200 HTTP server stands in for the daemon's local webhook handler.
	// These tests don't exercise forwarding — they assert on TOFU state only.
	localSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(localSrv.Close)
	localPort, err := extractPort(localSrv.URL)
	if err != nil {
		t.Fatalf("extract local port: %v", err)
	}

	logger := zaptest.NewLogger(t, zaptest.Level(zap.WarnLevel))
	client := relay.NewClient(h.wsBase, identity, localPort, database, logger)

	runCtx, cancel := context.WithCancel(context.Background())
	go func() { _ = client.Run(runCtx) }()

	if waitForWelcome {
		waitUntil(t, 15*time.Second, func() bool {
			return client.HookBaseURL() != ""
		}, "daemon never received welcome")
	}

	return client, cancel
}

// loadPin reads the stored broker pubkey from the daemon's TOFU table.
func loadPin(t *testing.T, database db.DB) string {
	t.Helper()
	pin, err := relay.LoadBrokerPubkey(context.Background(), database)
	if err != nil {
		t.Fatalf("load broker pin: %v", err)
	}
	return pin
}

// -----------------------------------------------------------------------------
// Scenario 2 — broker pubkey is pinned on first connect
// -----------------------------------------------------------------------------

func TestTOFU_PinOnFirstConnect(t *testing.T) {
	t.Parallel()
	keyPEM := mustGenerateBrokerKeyPEM(t)
	wantPub, err := brokerPubB64FromPEM(keyPEM)
	if err != nil {
		t.Fatalf("derive pubkey: %v", err)
	}

	h, advertisedPub := startRelayWithKey(t, keyPEM)
	defer h.terminate(t)
	if advertisedPub != wantPub {
		t.Fatalf("harness bug: advertisedPub %q != derived wantPub %q", advertisedPub, wantPub)
	}

	database := openTOFUDB(t)
	if got := loadPin(t, database); got != "" {
		t.Fatalf("pre-connect pin = %q, want empty", got)
	}

	client, cancel := startDaemonAgainst(t, h, database, relay.NewTestIdentity(t), true)
	defer cancel()

	if got := client.BrokerPubkey(); got != wantPub {
		t.Fatalf("client.BrokerPubkey() = %q, want %q", got, wantPub)
	}
	if got := loadPin(t, database); got != wantPub {
		t.Fatalf("stored pin = %q, want %q (CheckAndPinBrokerPubkey must store the advertised key on first connect)", got, wantPub)
	}
}

// -----------------------------------------------------------------------------
// Scenario 3 — reconnect after broker bounce preserves the pin
// -----------------------------------------------------------------------------
//
// Forces a reconnect by stopping and starting the SAME container
// (testcontainers-go preserves the mapped port across Stop/Start on the same
// handle). If the daemon re-pinned on every successful handshake instead of
// only on PinNew, the stored key would get overwritten by a re-advertised
// (identical) value — which is semantically a no-op today, but would break
// the invariant the moment the broker ever hands out a slightly different
// encoding of "the same key" (e.g. whitespace, encoding drift). Mutation-
// verified: forcing a re-pin on PinMatch would still pass this test because
// the replacement is identical; the real blast-radius check is scenario 4.
// Keep scenario 3 as a "did the daemon actually reconnect" sanity assertion.

func TestTOFU_PinPreservedAcrossReconnect(t *testing.T) {
	t.Parallel()
	keyPEM := mustGenerateBrokerKeyPEM(t)
	wantPub, err := brokerPubB64FromPEM(keyPEM)
	if err != nil {
		t.Fatalf("derive pubkey: %v", err)
	}

	h, _ := startRelayWithKey(t, keyPEM)
	defer h.terminate(t)

	database := openTOFUDB(t)
	client, cancel := startDaemonAgainst(t, h, database, relay.NewTestIdentity(t), true)
	defer cancel()

	if got := loadPin(t, database); got != wantPub {
		t.Fatalf("initial pin = %q, want %q", got, wantPub)
	}

	// Stop then start the same container. testcontainers preserves the
	// mapped port on the same handle, so the daemon's wsBase still points
	// at a reachable endpoint once the container is back up.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer stopCancel()
	stopTimeout := 5 * time.Second
	if err := h.container.Stop(stopCtx, &stopTimeout); err != nil {
		t.Fatalf("stop container: %v", err)
	}

	// While the container is down the daemon's Run loop retries with backoff.
	// Give it a brief moment to notice the disconnect.
	time.Sleep(500 * time.Millisecond)

	startCtx, startCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer startCancel()
	if err := h.container.Start(startCtx); err != nil {
		t.Fatalf("start container: %v", err)
	}
	// Re-wait for relay readiness via log — testcontainers' WaitingFor
	// from the initial create doesn't re-run on Start.
	readyCtx, readyCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer readyCancel()
	if err := waitForLog(readyCtx, h, "dicode-relay listening"); err != nil {
		t.Fatalf("wait for relay ready after restart: %v", err)
	}

	// Daemon's Run loop should reconnect within a few backoff cycles. The
	// relay is still advertising wantPub, so the handshake completes and
	// BrokerPubkey stays populated.
	waitUntil(t, 30*time.Second, func() bool {
		return client.BrokerPubkey() == wantPub
	}, "daemon did not reconnect and re-observe the broker pubkey")

	if got := loadPin(t, database); got != wantPub {
		t.Fatalf("post-reconnect pin = %q, want %q", got, wantPub)
	}
}

// -----------------------------------------------------------------------------
// Scenario 4 — reconnect refuses when broker presents a different key
// -----------------------------------------------------------------------------

func TestTOFU_RefusesAfterRotation(t *testing.T) {
	t.Parallel()

	origPEM := mustGenerateBrokerKeyPEM(t)
	origPub, err := brokerPubB64FromPEM(origPEM)
	if err != nil {
		t.Fatalf("derive origPub: %v", err)
	}
	newPEM := mustGenerateBrokerKeyPEM(t)
	newPub, err := brokerPubB64FromPEM(newPEM)
	if err != nil {
		t.Fatalf("derive newPub: %v", err)
	}
	if origPub == newPub {
		t.Fatal("freshly generated keys should differ")
	}

	// First daemon against a relay serving origPub: pins origPub, then stops.
	h1, _ := startRelayWithKey(t, origPEM)
	database := openTOFUDB(t)
	_, cancel1 := startDaemonAgainst(t, h1, database, relay.NewTestIdentity(t), true)
	if got := loadPin(t, database); got != origPub {
		cancel1()
		h1.terminate(t)
		t.Fatalf("initial pin = %q, want %q", got, origPub)
	}
	cancel1()
	h1.terminate(t)

	// Rotate: brand new container advertising newPub.
	h2, _ := startRelayWithKey(t, newPEM)
	defer h2.terminate(t)

	// Fresh daemon against h2 sharing the SAME db. Handshake should loop
	// in PinMismatch — never complete — so waitForWelcome=false.
	client, cancel2 := startDaemonAgainst(t, h2, database, relay.NewTestIdentity(t), false)
	defer cancel2()

	// A few backoff cycles are enough to exercise the mismatch path
	// multiple times without flaking.
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if client.BrokerPubkey() == newPub {
			t.Fatalf("client accepted rotated broker pubkey: BrokerPubkey() = %q; "+
				"the mismatch branch of CheckAndPinBrokerPubkey must abort the "+
				"handshake before updating the cached pubkey", newPub)
		}
		time.Sleep(100 * time.Millisecond)
	}

	if got := loadPin(t, database); got != origPub {
		t.Fatalf("DB pin overwritten on mismatch: got %q, want %q (original); "+
			"CheckAndPinBrokerPubkey must NOT overwrite on mismatch — that would "+
			"silently accept a rotated broker key and defeat TOFU", got, origPub)
	}
}

// -----------------------------------------------------------------------------
// Scenario 5 — `dicode relay trust-broker --yes` clears the pin and re-pins
// -----------------------------------------------------------------------------

func TestTOFU_TrustBrokerClearsPinAndRepins(t *testing.T) {
	t.Parallel()

	origPEM := mustGenerateBrokerKeyPEM(t)
	origPub, err := brokerPubB64FromPEM(origPEM)
	if err != nil {
		t.Fatalf("derive origPub: %v", err)
	}
	newPEM := mustGenerateBrokerKeyPEM(t)
	newPub, err := brokerPubB64FromPEM(newPEM)
	if err != nil {
		t.Fatalf("derive newPub: %v", err)
	}

	// Pin origPub via daemon1.
	h1, _ := startRelayWithKey(t, origPEM)
	database := openTOFUDB(t)
	_, cancel1 := startDaemonAgainst(t, h1, database, relay.NewTestIdentity(t), true)
	if got := loadPin(t, database); got != origPub {
		cancel1()
		h1.terminate(t)
		t.Fatalf("initial pin = %q, want %q", got, origPub)
	}
	cancel1()
	h1.terminate(t)

	// Rotate, then simulate the CLI: operator inspects the new key and
	// runs `dicode relay trust-broker --yes`, which calls
	// ReplaceBrokerPubkey. Daemon2 then handshakes successfully against
	// the rotated broker.
	h2, _ := startRelayWithKey(t, newPEM)
	defer h2.terminate(t)

	if err := relay.ReplaceBrokerPubkey(context.Background(), database, newPub); err != nil {
		t.Fatalf("ReplaceBrokerPubkey (simulated trust-broker CLI): %v", err)
	}

	client, cancel2 := startDaemonAgainst(t, h2, database, relay.NewTestIdentity(t), true)
	defer cancel2()

	if got := client.BrokerPubkey(); got != newPub {
		t.Fatalf("post-trust-broker BrokerPubkey() = %q, want %q", got, newPub)
	}
	if got := loadPin(t, database); got != newPub {
		t.Fatalf("post-trust-broker pin = %q, want %q", got, newPub)
	}
}

// waitForLog polls container logs until the given substring appears or ctx is
// cancelled. Used after Container.Start to re-establish readiness — the
// original WaitingFor matcher only fires on the initial create.
func waitForLog(ctx context.Context, h *relayHandle, needle string) error {
	nb := []byte(needle)
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %q: %w", needle, ctx.Err())
		default:
		}
		reader, err := h.container.Logs(ctx)
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		data, _ := io.ReadAll(reader)
		_ = reader.Close()
		if bytes.Contains(data, nb) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
}
