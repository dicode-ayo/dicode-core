package ipc

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/relay"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
	"golang.org/x/crypto/hkdf"
)

// memSecrets is a minimal secrets.Manager for tests.
type memSecrets struct {
	data map[string]string
}

func newMemSecrets() *memSecrets { return &memSecrets{data: map[string]string{}} }
func (m *memSecrets) List(_ context.Context) ([]string, error) {
	out := make([]string, 0, len(m.data))
	for k := range m.data {
		out = append(out, k)
	}
	return out, nil
}
func (m *memSecrets) Set(_ context.Context, k, v string) error { m.data[k] = v; return nil }
func (m *memSecrets) Delete(_ context.Context, k string) error { delete(m.data, k); return nil }
func (m *memSecrets) Get(_ context.Context, k string) (string, error) {
	return m.data[k], nil
}
func (m *memSecrets) Name() string { return "mem" }

var _ secrets.Manager = (*memSecrets)(nil)

// newOAuthIdentity delegates to relay.NewTestIdentity so the struct shape of
// relay.Identity stays an implementation detail of the relay package (see
// issue #104).
func newOAuthIdentity(t *testing.T) *relay.Identity {
	t.Helper()
	return relay.NewTestIdentity(t)
}

type oauthEnv struct {
	conn     net.Conn
	srv      *Server
	identity *relay.Identity
	pending  *relay.PendingSessions
	secrets  *memSecrets
}

// startOAuthServer wires an ipc.Server with both oauth capabilities granted
// and a shared PendingSessions store. Both permissions on one test server is
// fine — production splits init/store across two separate built-in tasks,
// but the server-side dispatch logic is identical regardless.
func startOAuthServer(t *testing.T) *oauthEnv {
	t.Helper()
	env := newTestEnv(t)
	spec := specWithDicode("auth-relay", &task.DicodePermissions{OAuthInit: true, OAuthStore: true})

	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	srv := New(runID, spec.ID, env.secret, env.reg, env.db, nil, nil, zap.NewNop(), spec, nil)
	identity := newOAuthIdentity(t)
	pending := relay.NewPendingSessions()
	secretsMgr := newMemSecrets()
	// supportsOAuthFn=nil so existing tests that pre-date #104 still exercise
	// the happy path without having to fake a handshake.
	srv.SetOAuthBroker(identity, "https://relay.dicode.app", pending, nil, nil)
	srv.SetSecrets(secretsMgr)

	socketPath, token, err := srv.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)

	conn := dial(t, socketPath)
	t.Cleanup(func() { conn.Close() })
	caps := doHandshake(t, conn, token)

	var init, store bool
	for _, c := range caps {
		if c == CapOAuthInit {
			init = true
		}
		if c == CapOAuthStore {
			store = true
		}
	}
	if !init || !store {
		t.Fatalf("expected both oauth.init and oauth.store caps; got %v", caps)
	}
	return &oauthEnv{conn: conn, srv: srv, identity: identity, pending: pending, secrets: secretsMgr}
}

func TestServer_OAuth_BuildAuthURL_RegistersPending(t *testing.T) {
	e := startOAuthServer(t)

	sendMsg(t, e.conn, map[string]any{
		"id":       "1",
		"method":   "dicode.oauth.build_auth_url",
		"provider": "slack",
		"scope":    "channels:read",
	})
	resp := recvMsg(t, e.conn)
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("bad result type: %T", resp["result"])
	}
	for _, k := range []string{"url", "session_id", "provider", "relay_uuid"} {
		if _, ok := result[k]; !ok {
			t.Fatalf("missing field %q: %v", k, result)
		}
	}
	if result["relay_uuid"] != e.identity.UUID {
		t.Fatalf("relay_uuid mismatch")
	}
	if e.pending.Len() != 1 {
		t.Fatalf("expected pending session to be registered; len=%d", e.pending.Len())
	}
}

func TestServer_OAuth_BuildAuthURL_DeniedWithoutInit(t *testing.T) {
	env := newTestEnv(t)
	// Spec has OAuthStore but NOT OAuthInit — build_auth_url must be rejected.
	spec := specWithDicode("leaky", &task.DicodePermissions{OAuthStore: true})
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	srv := New(runID, spec.ID, env.secret, env.reg, env.db, nil, nil, zap.NewNop(), spec, nil)
	srv.SetOAuthBroker(newOAuthIdentity(t), "https://relay.dicode.app", relay.NewPendingSessions(), nil, nil)
	socketPath, token, err := srv.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)
	conn := dial(t, socketPath)
	t.Cleanup(func() { conn.Close() })
	doHandshake(t, conn, token)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.oauth.build_auth_url", "provider": "slack"})
	resp := recvMsg(t, conn)
	if errStr, _ := resp["error"].(string); errStr == "" {
		t.Fatalf("expected permission denied")
	}
}

func TestServer_OAuth_StoreToken_RoundTrip(t *testing.T) {
	e := startOAuthServer(t)

	// Issue an auth request so the pending session exists.
	sendMsg(t, e.conn, map[string]any{"id": "1", "method": "dicode.oauth.build_auth_url", "provider": "slack"})
	initResp := recvMsg(t, e.conn)
	sessionID := initResp["result"].(map[string]any)["session_id"].(string)

	tokenJSON := []byte(`{"access_token":"xoxb-1","refresh_token":"r1","expires_in":3600,"scope":"channels:read","token_type":"bot"}`)
	envelope := sealForDaemon(t, e.identity, sessionID, tokenJSON)
	envBytes, _ := json.Marshal(envelope)

	sendMsg(t, e.conn, map[string]any{
		"id":       "2",
		"method":   "dicode.oauth.store_token",
		"envelope": json.RawMessage(envBytes),
	})
	resp := recvMsg(t, e.conn)
	if resp["error"] != nil {
		t.Fatalf("unexpected error: %v", resp["error"])
	}
	if e.secrets.data["SLACK_ACCESS_TOKEN"] != "xoxb-1" {
		t.Fatalf("access token not stored: %+v", e.secrets.data)
	}
	if e.secrets.data["SLACK_REFRESH_TOKEN"] != "r1" {
		t.Fatalf("refresh token not stored")
	}
	if e.secrets.data["SLACK_SCOPE"] != "channels:read" {
		t.Fatalf("scope not stored")
	}
	if e.secrets.data["SLACK_TOKEN_TYPE"] != "bot" {
		t.Fatalf("token_type not stored")
	}
	exp := e.secrets.data["SLACK_EXPIRES_AT"]
	if _, err := time.Parse(time.RFC3339, exp); err != nil {
		t.Fatalf("expires_at not RFC3339: %q", exp)
	}
	if e.pending.Len() != 0 {
		t.Fatalf("pending session should be consumed; len=%d", e.pending.Len())
	}
}

func TestServer_OAuth_StoreToken_UnknownSession(t *testing.T) {
	e := startOAuthServer(t)

	// Do not register a pending session. Craft an envelope with an
	// arbitrary session id — store_token must reject it even though
	// decryption would succeed.
	sessionID := "550e8400-e29b-41d4-a716-446655440000"
	envelope := sealForDaemon(t, e.identity, sessionID, []byte(`{"access_token":"x"}`))
	envBytes, _ := json.Marshal(envelope)

	sendMsg(t, e.conn, map[string]any{
		"id":       "1",
		"method":   "dicode.oauth.store_token",
		"envelope": json.RawMessage(envBytes),
	})
	resp := recvMsg(t, e.conn)
	if errStr, _ := resp["error"].(string); errStr == "" {
		t.Fatalf("expected unknown-session error")
	}
	if len(e.secrets.data) != 0 {
		t.Fatalf("secrets should be untouched on rejection")
	}
}

func TestServer_OAuth_StoreToken_TamperedCiphertext(t *testing.T) {
	e := startOAuthServer(t)

	sendMsg(t, e.conn, map[string]any{"id": "1", "method": "dicode.oauth.build_auth_url", "provider": "slack"})
	initResp := recvMsg(t, e.conn)
	sessionID := initResp["result"].(map[string]any)["session_id"].(string)

	envelope := sealForDaemon(t, e.identity, sessionID, []byte(`{"access_token":"x"}`))
	// Flip the ciphertext.
	ct, _ := base64.StdEncoding.DecodeString(envelope.Ciphertext)
	ct[0] ^= 0xFF
	envelope.Ciphertext = base64.StdEncoding.EncodeToString(ct)
	envBytes, _ := json.Marshal(envelope)

	sendMsg(t, e.conn, map[string]any{
		"id":       "2",
		"method":   "dicode.oauth.store_token",
		"envelope": json.RawMessage(envBytes),
	})
	resp := recvMsg(t, e.conn)
	if errStr, _ := resp["error"].(string); errStr == "" {
		t.Fatalf("expected decrypt error")
	}
	// Tampered delivery MUST still evict the pending session — otherwise a
	// retry loop could brute-force something.
	if e.pending.Len() != 0 {
		t.Fatalf("pending session should be consumed on tamper")
	}
}

func TestServer_OAuth_StoreToken_RejectsEmptyType(t *testing.T) {
	e := startOAuthServer(t)

	sendMsg(t, e.conn, map[string]any{"id": "1", "method": "dicode.oauth.build_auth_url", "provider": "slack"})
	initResp := recvMsg(t, e.conn)
	sessionID := initResp["result"].(map[string]any)["session_id"].(string)

	envelope := sealForDaemon(t, e.identity, sessionID, []byte(`{"access_token":"x"}`))
	envelope.Type = "" // strip the type tag
	envBytes, _ := json.Marshal(envelope)

	sendMsg(t, e.conn, map[string]any{
		"id":       "2",
		"method":   "dicode.oauth.store_token",
		"envelope": json.RawMessage(envBytes),
	})
	resp := recvMsg(t, e.conn)
	if errStr, _ := resp["error"].(string); errStr == "" {
		t.Fatalf("expected type-required error")
	}
}

func TestServer_OAuth_NotConfigured(t *testing.T) {
	env := newTestEnv(t)
	spec := specWithDicode("auth-relay", &task.DicodePermissions{OAuthInit: true})
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	// Do NOT call SetOAuthBroker — oauth.* should report not-configured.
	srv := New(runID, spec.ID, env.secret, env.reg, env.db, nil, nil, zap.NewNop(), spec, nil)
	socketPath, token, err := srv.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)
	conn := dial(t, socketPath)
	t.Cleanup(func() { conn.Close() })
	doHandshake(t, conn, token)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.oauth.build_auth_url", "provider": "slack"})
	resp := recvMsg(t, conn)
	if errStr, _ := resp["error"].(string); errStr == "" {
		t.Fatalf("expected not-configured error")
	}
}

// TestServer_OAuth_RefusesWhenBrokerProtocolOld covers decision 3 of the
// issue #104 review: when the connected broker has advertised a protocol
// version < 2 (captured here by supportsOAuthFn=false), both
// dicode.oauth.build_auth_url and dicode.oauth.store_token must refuse
// the request with the clear "upgrade dicode-relay to protocol >= 2"
// error rather than silently proceeding to a mismatched-key decrypt.
func TestServer_OAuth_RefusesWhenBrokerProtocolOld(t *testing.T) {
	env := newTestEnv(t)
	spec := specWithDicode("auth-relay", &task.DicodePermissions{OAuthInit: true, OAuthStore: true})
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	srv := New(runID, spec.ID, env.secret, env.reg, env.db, nil, nil, zap.NewNop(), spec, nil)

	id := newOAuthIdentity(t)
	pending := relay.NewPendingSessions()
	secretsMgr := newMemSecrets()
	// supportsOAuthFn always false — simulating a broker still on protocol 1.
	srv.SetOAuthBroker(id, "https://relay.dicode.app", pending, nil, func() bool { return false })
	srv.SetSecrets(secretsMgr)

	socketPath, token, err := srv.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(srv.Stop)
	conn := dial(t, socketPath)
	t.Cleanup(func() { conn.Close() })
	doHandshake(t, conn, token)

	// build_auth_url must be refused with the clear upgrade message.
	sendMsg(t, conn, map[string]any{"id": "1", "method": "dicode.oauth.build_auth_url", "provider": "slack"})
	resp := recvMsg(t, conn)
	errStr, _ := resp["error"].(string)
	if errStr == "" {
		t.Fatal("expected broker-protocol error, got none")
	}
	if !strings.Contains(errStr, "protocol") || !strings.Contains(errStr, "2") {
		t.Fatalf("expected operator-actionable protocol error, got: %q", errStr)
	}
	// The pending store must not have grown — the gate is pre-enqueue.
	if pending.Len() != 0 {
		t.Fatalf("pending session unexpectedly registered: len=%d", pending.Len())
	}

	// store_token is also refused, even before decryption is attempted. We
	// pass a deliberately malformed envelope — the gate must short-circuit
	// before any decode happens.
	sendMsg(t, conn, map[string]any{
		"id":       "2",
		"method":   "dicode.oauth.store_token",
		"envelope": json.RawMessage(`{}`),
	})
	resp2 := recvMsg(t, conn)
	errStr2, _ := resp2["error"].(string)
	if errStr2 == "" {
		t.Fatal("expected broker-protocol error on store_token, got none")
	}
	if !strings.Contains(errStr2, "protocol") {
		t.Fatalf("expected protocol error on store_token, got: %q", errStr2)
	}
}

// sealForDaemon mirrors dicode-relay src/broker/crypto.ts eciesEncrypt.
// Post-#104 the broker encrypts against the daemon's DecryptKey pubkey —
// mirror that here so the test actually exercises the production code path.
func sealForDaemon(t *testing.T, identity *relay.Identity, sessionID string, plaintext []byte) *relay.OAuthTokenDeliveryPayload {
	t.Helper()
	daemonPub, err := ecdh.P256().NewPublicKey(identity.DecryptPublicKey())
	if err != nil {
		t.Fatalf("daemon pub: %v", err)
	}
	eph, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("eph: %v", err)
	}
	shared, err := eph.ECDH(daemonPub)
	if err != nil {
		t.Fatalf("ecdh: %v", err)
	}
	encKey := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, shared, []byte(sessionID), []byte("dicode-oauth-token")), encKey); err != nil {
		t.Fatalf("hkdf: %v", err)
	}
	iv := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		t.Fatalf("iv: %v", err)
	}
	block, _ := aes.NewCipher(encKey)
	aead, _ := cipher.NewGCM(block)
	ct := aead.Seal(nil, iv, plaintext, []byte("oauth_token_delivery"))
	return &relay.OAuthTokenDeliveryPayload{
		Type:            "oauth_token_delivery",
		SessionID:       sessionID,
		EphemeralPubkey: base64.StdEncoding.EncodeToString(eph.PublicKey().Bytes()),
		Ciphertext:      base64.StdEncoding.EncodeToString(ct),
		Nonce:           base64.StdEncoding.EncodeToString(iv),
	}
}
