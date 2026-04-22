//go:build e2e

// Package relay_e2e exercises the dicode daemon (in-process, via the real
// pkg/relay and pkg/ipc APIs) against a live dicode-relay container spun up
// by testcontainers-go. The suite catches cross-service protocol drift that
// unit tests on either side cannot.
//
// Pin: the relay image version lives in testdata/Dockerfile.relay — the
// `FROM` line is the single source of truth and is tracked by Dependabot's
// docker ecosystem. testcontainers-go's `FromDockerfile` builds the image
// on the fly (no RUN steps → cheap pull + tag) so a dependabot bump of the
// Dockerfile is picked up by the next test run with no code change.
//
// Run with: `make test-e2e-relay` (Docker must be available on the host).
package relay_e2e

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/relay"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

// -----------------------------------------------------------------------------
// Test harness
// -----------------------------------------------------------------------------

// relayHandle bundles a running relay container with the endpoints the tests
// need to reach it (WS for the daemon, HTTP for the browser-flow simulator).
type relayHandle struct {
	container testcontainers.Container
	httpBase  string // e.g. http://127.0.0.1:54321
	wsBase    string // e.g. ws://127.0.0.1:54321
}

func (h *relayHandle) terminate(t *testing.T) {
	t.Helper()
	if h == nil || h.container == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.container.Terminate(ctx); err != nil {
		t.Logf("relay: terminate failed: %v", err)
	}
}

// startRelay builds and runs the relay container from testdata/Dockerfile.relay
// with the e2e mock provider enabled. Returns a handle to the running
// container and its mapped endpoints.
func startRelay(t *testing.T) *relayHandle {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	absTestdata, err := filepath.Abs("testdata")
	if err != nil {
		t.Fatalf("resolve testdata path: %v", err)
	}

	req := testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			FromDockerfile: testcontainers.FromDockerfile{
				Context:       absTestdata,
				Dockerfile:    "Dockerfile.relay",
				PrintBuildLog: false,
				KeepImage:     true,
			},
			ExposedPorts: []string{"5553/tcp"},
			// relay.yaml is COPY'd into the image at build time by
			// Dockerfile.relay — baking it in avoids a post-start file
			// copy race that would let the relay process exit on "config
			// not found" before testcontainers streamed the yaml in.
			Env: map[string]string{
				"DICODE_E2E_MOCK_PROVIDER": "1",
				// The published image's Dockerfile bakes in NODE_ENV=production
				// as a defense-in-depth layer — isE2EMockEnabled() refuses to
				// register the mock provider under production even if the
				// flag above is set. Override here so the test harness can
				// actually exercise /auth/mock. Safe: these tests only run
				// under the `e2e` build tag, never in a prod-deployed binary.
				"NODE_ENV": "test",
				// The container runs as USER dicode and cannot write to
				// /app, so the relay's default auto-generate-to-cwd path
				// fails with EACCES. Inject a pre-baked ECDSA P-256 PEM
				// via env instead; also gives tests control over the
				// broker sig key for scenarios 3/7 (split-key regression
				// + TOFU rotation).
				"BROKER_SIGNING_KEY": mustGenerateBrokerKeyPEM(t),
			},
			WaitingFor: wait.ForLog("dicode-relay listening").WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	}

	container, err := testcontainers.GenericContainer(ctx, req)
	if err != nil {
		t.Fatalf("start relay container: %v", err)
	}

	host, err := container.Host(ctx)
	if err != nil {
		container.Terminate(ctx) //nolint:errcheck
		t.Fatalf("container host: %v", err)
	}
	port, err := container.MappedPort(ctx, "5553/tcp")
	if err != nil {
		container.Terminate(ctx) //nolint:errcheck
		t.Fatalf("mapped port: %v", err)
	}

	return &relayHandle{
		container: container,
		httpBase:  fmt.Sprintf("http://%s:%s", host, port.Port()),
		wsBase:    fmt.Sprintf("ws://%s:%s", host, port.Port()),
	}
}

// daemonHandle bundles the pieces of a running daemon: the relay.Client driving
// the WS connection, the local HTTP server the relay forwards into, and the
// identity + db backing them.
type daemonHandle struct {
	client    *relay.Client
	identity  *relay.Identity
	localPort int
	localSrv  *httptest.Server
	cancelRun context.CancelFunc
	runErrCh  chan error
}

func (d *daemonHandle) stop(t *testing.T) {
	t.Helper()
	if d == nil {
		return
	}
	if d.cancelRun != nil {
		d.cancelRun()
	}
	if d.runErrCh != nil {
		select {
		case <-d.runErrCh:
		case <-time.After(5 * time.Second):
			t.Log("daemon: Run did not exit within 5s")
		}
	}
	if d.localSrv != nil {
		d.localSrv.Close()
	}
}

// startDaemon wires a real relay.Client against the given relay handle.
//
// localHandler is the http.Handler the relay will forward inbound webhook
// requests into — exercising the full WSS → forward → local HTTP round-trip.
// For OAuth tests, intercept /hooks/oauth-complete here to capture the
// delivery envelope.
func startDaemon(t *testing.T, h *relayHandle, localHandler http.Handler) *daemonHandle {
	t.Helper()

	srv := httptest.NewServer(localHandler)
	localPort, err := extractPort(srv.URL)
	if err != nil {
		srv.Close()
		t.Fatalf("extract local port: %v", err)
	}

	// In-memory SQLite for the daemon's TOFU broker pin. A temp file would
	// do the same thing with fewer lines, but :memory: keeps the test
	// hermetic — no cleanup, no cross-test contamination.
	database, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		srv.Close()
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	identity := relay.NewTestIdentity(t)

	logger := zaptest.NewLogger(t, zaptest.Level(zap.WarnLevel))
	client := relay.NewClient(h.wsBase, identity, localPort, database, logger)

	runCtx, cancel := context.WithCancel(context.Background())
	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- client.Run(runCtx)
	}()

	// Wait for the handshake to complete: HookBaseURL is only populated
	// after a successful welcome.
	waitUntil(t, 15*time.Second, func() bool {
		return client.HookBaseURL() != ""
	}, "daemon never received welcome")

	return &daemonHandle{
		client:    client,
		identity:  identity,
		localPort: localPort,
		localSrv:  srv,
		cancelRun: cancel,
		runErrCh:  runErrCh,
	}
}

// -----------------------------------------------------------------------------
// Scenario 1: happy path — handshake, protocol v2, HTTP forward round-trip
// -----------------------------------------------------------------------------

func TestHappyPath_HandshakeAndForward(t *testing.T) {
	t.Parallel()
	h := startRelay(t)
	defer h.terminate(t)

	// Local HTTP handler the daemon stands behind. Echoes the request path
	// so the test can assert the forwarded path was preserved end-to-end.
	mux := http.NewServeMux()
	mux.HandleFunc("/hooks/e2e-echo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, _ := io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"path":   r.URL.Path,
			"method": r.Method,
			"body":   string(body),
		})
	})

	d := startDaemon(t, h, mux)
	defer d.stop(t)

	// Sanity: post-handshake invariants from #104.
	if !d.client.SupportsOAuth() {
		t.Errorf("SupportsOAuth() = false; want true (relay should advertise protocol: 2)")
	}
	if d.client.BrokerPubkey() == "" {
		t.Errorf("BrokerPubkey() empty after handshake; TOFU pin should be set")
	}
	// HookBaseURL comes from the relay's welcome message, which uses the
	// broker's configured base_url — inside the container that's
	// http://localhost:5553, not the test-reachable mapped address. Just
	// assert it carries the daemon's UUID; use h.httpBase below for the
	// test's own HTTP calls.
	hook := d.client.HookBaseURL()
	if !strings.Contains(hook, "/u/"+d.identity.UUID+"/hooks/") {
		t.Errorf("HookBaseURL = %q; want to contain /u/<uuid>/hooks/", hook)
	}

	// End-to-end: hit the relay's public hook URL (via the mapped port)
	// → forwarded over WS → handled by our local HTTP server → response
	// shipped back.
	externalHook := h.httpBase + "/u/" + d.identity.UUID + "/hooks/e2e-echo"
	reqBody := []byte(`{"hello":"world"}`)
	req, _ := http.NewRequest(http.MethodPost, externalHook, bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST to hook URL: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("hook forward status = %d; want 200; body=%s", resp.StatusCode, respBody)
	}
	var echoed struct {
		Path   string `json:"path"`
		Method string `json:"method"`
		Body   string `json:"body"`
	}
	if err := json.Unmarshal(respBody, &echoed); err != nil {
		t.Fatalf("decode echo: %v; body=%s", err, respBody)
	}
	if echoed.Path != "/hooks/e2e-echo" {
		t.Errorf("forwarded path = %q; want /hooks/e2e-echo", echoed.Path)
	}
	if echoed.Method != http.MethodPost {
		t.Errorf("forwarded method = %q; want POST", echoed.Method)
	}
	if echoed.Body != string(reqBody) {
		t.Errorf("forwarded body = %q; want %q", echoed.Body, string(reqBody))
	}
}

// -----------------------------------------------------------------------------
// Scenario 2: OAuth happy path — build_auth_url → /connect/mock → ECIES delivery
// -----------------------------------------------------------------------------

func TestOAuthHappyPath_MockProvider(t *testing.T) {
	t.Parallel()
	h := startRelay(t)
	defer h.terminate(t)

	// Capture the delivery envelope that the broker forwards into the
	// daemon's /hooks/oauth-complete. We decrypt it inline and assert the
	// plaintext — bypassing the trigger-engine secrets write (covered by
	// dicode-core's own unit tests). The value-add of this scenario is
	// proving the wire path: broker sig verify, ECIES encrypt+decrypt,
	// session-id binding.
	var captured struct {
		payload *relay.OAuthTokenDeliveryPayload
		err     error
		done    chan struct{}
	}
	captured.done = make(chan struct{}, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/hooks/oauth-complete", func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			select {
			case captured.done <- struct{}{}:
			default:
			}
		}()
		var p relay.OAuthTokenDeliveryPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			captured.err = fmt.Errorf("decode delivery: %w", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		captured.payload = &p
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	d := startDaemon(t, h, mux)
	defer d.stop(t)

	// 1. Daemon builds the signed /auth/mock URL pointing at the broker.
	authURL, authReq, err := relay.BuildAuthURL(h.httpBase, d.identity, "mock", "", time.Now().Unix())
	if err != nil {
		t.Fatalf("BuildAuthURL: %v", err)
	}
	if authReq.SessionID == "" {
		t.Fatalf("auth request missing session id")
	}

	// 2. Drive the browser-flow by following redirects. The chain is:
	//    /auth/mock → /connect/mock (handled by e2e-mock router) →
	//    /callback/mock (handled by broker router, which encrypts +
	//    signs + forwards to the daemon).
	httpClient := &http.Client{
		// Follow all redirects inside the relay process. These are always
		// relative paths so the client sticks to the container's host.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects (%d) — relay flow is stuck", len(via))
			}
			return nil
		},
		Timeout: 10 * time.Second,
	}
	resp, err := httpClient.Get(authURL)
	if err != nil {
		t.Fatalf("follow auth flow: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("auth flow final status = %d; want 2xx; body=%s", resp.StatusCode, body)
	}

	// 3. Wait for the delivery to arrive at the daemon.
	select {
	case <-captured.done:
	case <-time.After(10 * time.Second):
		t.Fatal("delivery never reached /hooks/oauth-complete")
	}
	if captured.err != nil {
		t.Fatalf("delivery decode: %v", captured.err)
	}
	if captured.payload == nil {
		t.Fatal("delivery payload nil after capture")
	}

	// 4. Invariants on the envelope before decryption.
	if got, want := captured.payload.SessionID, authReq.SessionID; got != want {
		t.Errorf("delivery session_id = %q; want %q", got, want)
	}
	if captured.payload.Type != "oauth_token_delivery" {
		t.Errorf("delivery type = %q; want oauth_token_delivery", captured.payload.Type)
	}
	if captured.payload.BrokerSig == "" {
		t.Error("delivery missing broker_sig (#152)")
	}

	// 5. Verify the broker signature against the TOFU-pinned pubkey.
	brokerPub := d.client.BrokerPubkey()
	if brokerPub == "" {
		t.Fatal("daemon has no pinned broker pubkey after handshake")
	}
	if err := relay.VerifyBrokerSig(brokerPub, captured.payload); err != nil {
		t.Fatalf("VerifyBrokerSig (cross-impl check — matches #151 fix): %v", err)
	}

	// 6. ECIES-decrypt with the daemon's decrypt key (split from sign per #104).
	plaintext, err := relay.DecryptOAuthToken(d.identity, captured.payload)
	if err != nil {
		t.Fatalf("DecryptOAuthToken: %v", err)
	}

	// 7. The mock provider hands back {access_token, token_type} as query
	//    params that Grant-shaped /callback handlers echo into the tokens
	//    object. Assert the marker token the e2e-mock router synthesises.
	wantToken := "mock-token-" + authReq.SessionID
	if !bytes.Contains(plaintext, []byte(wantToken)) {
		t.Errorf("decrypted plaintext does not contain %q; got %s", wantToken, string(plaintext))
	}
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// mustGenerateBrokerKeyPEM returns a freshly generated ECDSA P-256 private
// key in PKCS8 PEM form, suitable for the relay's BROKER_SIGNING_KEY env
// var. Each test gets its own key — the container-baked public half ends
// up TOFU-pinned by the daemon during handshake, and tests that need to
// rotate the broker key (scenarios 3/7) can re-invoke this and restart
// the container.
func mustGenerateBrokerKeyPEM(t *testing.T) string {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate broker key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// waitUntil polls cond every 50ms until it returns true or the deadline
// elapses. Fails the test with msg if the deadline is reached.
func waitUntil(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("waitUntil timeout after %s: %s", d, msg)
}

// extractPort pulls the integer port out of an http://host:port URL. The
// httptest.Server gives us one of these on every call to httptest.NewServer.
func extractPort(rawURL string) (int, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, err
	}
	_, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		return 0, err
	}
	var p int
	if _, err := fmt.Sscanf(portStr, "%d", &p); err != nil {
		return 0, err
	}
	return p, nil
}
