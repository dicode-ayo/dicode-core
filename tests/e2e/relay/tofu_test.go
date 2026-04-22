//go:build e2e

package relay_e2e

import (
	"context"
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

// daemon bundles a running relay.Client + its cancel + a `done` channel
// that closes once the goroutine running Client.Run has returned.
type daemon struct {
	client *relay.Client
	cancel context.CancelFunc
	done   <-chan struct{}
}

// stop cancels the daemon's context and waits for the Run goroutine to
// return. Sequential-daemon tests MUST call stop (not just cancel)
// before starting the next daemon on the same database, otherwise the
// outgoing goroutine can still be mid-handshake when the next one
// starts — shared SQLite writes stay serialised but error-path
// ordering becomes racy.
func (d *daemon) stop(t *testing.T) {
	t.Helper()
	d.cancel()
	select {
	case <-d.done:
	case <-time.After(5 * time.Second):
		t.Log("daemon: Run did not exit within 5s after cancel")
	}
}

// startDaemonAgainst wires a real relay.Client to h.wsBase, using the
// given db (for TOFU pin persistence across daemon lifecycles) and
// identity. If waitForWelcome is true, blocks until the handshake
// completes (HookBaseURL populated).
func startDaemonAgainst(
	t *testing.T,
	h *relayHandle,
	database db.DB,
	identity *relay.Identity,
	waitForWelcome bool,
) *daemon {
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
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := client.Run(runCtx); err != nil {
			// Run only returns nil on ctx cancel; any other error is surprising
			// and worth surfacing in test logs so failures aren't mysterious.
			t.Logf("daemon: Run returned: %v", err)
		}
	}()

	if waitForWelcome {
		waitUntil(t, 15*time.Second, func() bool {
			return client.HookBaseURL() != ""
		}, "daemon never received welcome")
	}

	return &daemon{client: client, cancel: cancel, done: done}
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

	d := startDaemonAgainst(t, h, database, relay.NewTestIdentity(t), true)
	defer d.stop(t)

	if got := d.client.BrokerPubkey(); got != wantPub {
		t.Fatalf("client.BrokerPubkey() = %q, want %q", got, wantPub)
	}
	if got := loadPin(t, database); got != wantPub {
		t.Fatalf("stored pin = %q, want %q (CheckAndPinBrokerPubkey must store the advertised key on first connect)", got, wantPub)
	}
}

// Note on scenario 3 ("reconnect preserves pin"): deliberately NOT shipped in
// this file. The most faithful implementation — Stop+Start the same container
// and observe the daemon's internal reconnect — is unreliable on CI because
// the daemon's exponential backoff doubles on each failed dial during the
// container-down window, easily pushing the first successful reconnect past
// any reasonable test timeout. And the semantic it was guarding —
// CheckAndPinBrokerPubkey's PinMatch branch must not overwrite — is currently
// a tautology: the PinMatch path has zero DB writes to begin with. Scenarios
// 4 and 5 exercise the DB-writing branches end-to-end (PinMismatch rejects,
// ReplaceBrokerPubkey reassigns); they're the load-bearing tests. If future
// work adds metadata writes to PinMatch (e.g. last-seen-at), reintroduce this
// scenario via a non-backoff-dependent harness (container exec to restart
// just the Node process, or direct socket sever on the daemon's Client).

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

	// First daemon against a relay serving origPub: pins origPub, then
	// stops. stop() waits for the Run goroutine to exit so we don't
	// share the database with a still-running daemon-1 when daemon-2
	// starts — avoids a race on the shared kv table even though
	// SQLite serialises individual writes.
	h1, _ := startRelayWithKey(t, origPEM)
	database := openTOFUDB(t)
	d1 := startDaemonAgainst(t, h1, database, relay.NewTestIdentity(t), true)
	if got := loadPin(t, database); got != origPub {
		d1.stop(t)
		h1.terminate(t)
		t.Fatalf("initial pin = %q, want %q", got, origPub)
	}
	d1.stop(t)
	h1.terminate(t)

	// Rotate: brand new container advertising newPub.
	h2, _ := startRelayWithKey(t, newPEM)
	defer h2.terminate(t)

	// Fresh daemon against h2 sharing the SAME db. Handshake should loop
	// in PinMismatch — never complete — so waitForWelcome=false.
	d2 := startDaemonAgainst(t, h2, database, relay.NewTestIdentity(t), false)
	defer d2.stop(t)

	// A few backoff cycles are enough to exercise the mismatch path
	// multiple times without flaking.
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if d2.client.BrokerPubkey() == newPub {
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

	// Pin origPub via daemon1; stop it (and wait for its goroutine to
	// exit) before daemon2 starts against the same database.
	h1, _ := startRelayWithKey(t, origPEM)
	database := openTOFUDB(t)
	d1 := startDaemonAgainst(t, h1, database, relay.NewTestIdentity(t), true)
	if got := loadPin(t, database); got != origPub {
		d1.stop(t)
		h1.terminate(t)
		t.Fatalf("initial pin = %q, want %q", got, origPub)
	}
	d1.stop(t)
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

	d2 := startDaemonAgainst(t, h2, database, relay.NewTestIdentity(t), true)
	defer d2.stop(t)

	if got := d2.client.BrokerPubkey(); got != newPub {
		t.Fatalf("post-trust-broker BrokerPubkey() = %q, want %q", got, newPub)
	}
	if got := loadPin(t, database); got != newPub {
		t.Fatalf("post-trust-broker pin = %q, want %q", got, newPub)
	}
}
