package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	denoruntime "github.com/dicode/dicode/pkg/runtime/deno"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/trigger"
	"go.uber.org/zap"
)

// inMemSecretsMgr is a minimal in-memory secrets.Manager for testing.
// The real LocalProvider requires a master key on disk + Argon2id derivation,
// which is overkill for API-surface tests — we only need the three CRUD
// methods the handlers call through.
type inMemSecretsMgr struct {
	mu   sync.Mutex
	data map[string]string
}

func (m *inMemSecretsMgr) List(_ context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.data))
	for k := range m.data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys, nil
}

func (m *inMemSecretsMgr) Set(_ context.Context, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data == nil {
		m.data = map[string]string{}
	}
	m.data[key] = value
	return nil
}

func (m *inMemSecretsMgr) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	return nil
}

func (m *inMemSecretsMgr) get(key string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data[key]
}

// newSecretsTestServer spins up a webui Server wired to an in-memory secrets
// manager. server.auth is false, so the protected endpoints are reachable
// without a session — this test targets the response shape, not auth.
func newSecretsTestServer(t *testing.T) (*Server, *inMemSecretsMgr) {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	reg := registry.New(d)
	rt, err := denoruntime.New(reg, secrets.Chain{}, d, zap.NewNop())
	if err != nil {
		t.Skipf("deno runtime not available: %v", err)
	}
	eng := trigger.New(reg, rt, zap.NewNop())
	mgr := &inMemSecretsMgr{}

	var sm secrets.Manager = mgr
	srv, err := New(
		0, // port — unused (we call Handler() directly)
		reg,
		eng,
		&config.Config{Server: config.ServerConfig{Port: 0, Auth: false}},
		"", sm, nil, nil, "",
		NewLogBroadcaster(),
		zap.NewNop(),
		d,
		ipc.NewGateway(),
	)
	if err != nil {
		t.Fatalf("New server: %v", err)
	}
	return srv, mgr
}

// TestAPI_ListSecrets_ReturnsKeysOnly verifies #126 item 1:
// GET /api/secrets returns key names only — values must never appear in the
// response body, even for authenticated admins.
func TestAPI_ListSecrets_ReturnsKeysOnly(t *testing.T) {
	srv, mgr := newSecretsTestServer(t)

	// Seed with keys whose values contain distinctive markers that would be
	// obvious if they leaked into the response body.
	_ = mgr.Set(context.Background(), "api_key", "sk-LEAK-MARKER-AAA")
	_ = mgr.Set(context.Background(), "db_password", "LEAK-MARKER-BBB")
	_ = mgr.Set(context.Background(), "webhook_secret", "LEAK-MARKER-CCC")

	req := httptest.NewRequest(http.MethodGet, "/api/secrets", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", w.Code, w.Body.String())
	}

	body := w.Body.String()
	for _, marker := range []string{"LEAK-MARKER-AAA", "LEAK-MARKER-BBB", "LEAK-MARKER-CCC", "sk-LEAK"} {
		if strings.Contains(body, marker) {
			t.Errorf("response body contains secret value marker %q\nfull body: %s", marker, body)
		}
	}

	var keys []string
	if err := json.Unmarshal(w.Body.Bytes(), &keys); err != nil {
		t.Fatalf("decode body: %v (body=%q)", err, body)
	}
	// Set-based assertion — the real LocalProvider doesn't guarantee an
	// ordering (SQLite scan order varies), so the test must not depend on
	// the mock's `sort.Strings` helper.
	want := map[string]bool{"api_key": true, "db_password": true, "webhook_secret": true}
	if len(keys) != len(want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	for _, k := range keys {
		if !want[k] {
			t.Errorf("unexpected key %q in response", k)
		}
		delete(want, k)
	}
	if len(want) > 0 {
		t.Errorf("missing keys in response: %v", want)
	}
}

// TestAPI_ListSecrets_EmptyStore verifies an empty list returns JSON `[]`,
// not `null` — consumers iterate the array and null would break them.
func TestAPI_ListSecrets_EmptyStore(t *testing.T) {
	srv, _ := newSecretsTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/secrets", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	// Either `[]` or `null` is JSON-valid; we want the empty-array form so
	// the SPA can `.map(...)` without a null-guard.
	body := strings.TrimSpace(w.Body.String())
	if body != "[]" {
		t.Errorf("empty list body = %q, want %q", body, "[]")
	}
}

// TestAPI_SetSecret_IsWriteOnly verifies #126 item 2:
// POST /api/secrets stores the value but does NOT echo it in the response.
// The response body must only contain acknowledgement metadata.
func TestAPI_SetSecret_IsWriteOnly(t *testing.T) {
	srv, mgr := newSecretsTestServer(t)

	const secretKey = "github_token"
	const secretValue = "ghp_LEAK-MARKER-NEVER-ECHOED-12345"

	body, _ := json.Marshal(map[string]string{"key": secretKey, "value": secretValue})
	req := httptest.NewRequest(http.MethodPost, "/api/secrets", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", w.Code, w.Body.String())
	}

	respBody := w.Body.String()
	if strings.Contains(respBody, "LEAK-MARKER") || strings.Contains(respBody, secretValue) {
		t.Errorf("POST response echoes secret value\nbody: %s", respBody)
	}

	// Verify storage did happen — the value should be retrievable from the
	// backing manager (the path the daemon uses internally), not via API.
	stored := mgr.get(secretKey)
	if stored != secretValue {
		t.Errorf("stored value = %q, want %q", stored, secretValue)
	}
}

// TestAPI_SetSecret_RejectsEmptyKey verifies the existing guard in
// apiSetSecret. Included here because #126 asks for secret-handling
// regression coverage and this path is adjacent.
func TestAPI_SetSecret_RejectsEmptyKey(t *testing.T) {
	srv, _ := newSecretsTestServer(t)

	body, _ := json.Marshal(map[string]string{"key": "", "value": "v"})
	req := httptest.NewRequest(http.MethodPost, "/api/secrets", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%q", w.Code, w.Body.String())
	}
}

// TestAPI_ListSecrets_AfterSetDoesNotIncludeValues is the full round-trip:
// POST a secret, then GET the list, verify only the key is visible and the
// value never appears anywhere in the list response.
func TestAPI_ListSecrets_AfterSetDoesNotIncludeValues(t *testing.T) {
	srv, _ := newSecretsTestServer(t)

	const secretValue = "oauth_LEAK-FOLLOWUP-MARKER"
	body, _ := json.Marshal(map[string]string{"key": "oauth_token", "value": secretValue})
	req := httptest.NewRequest(http.MethodPost, "/api/secrets", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	srv.Handler().ServeHTTP(httptest.NewRecorder(), req)

	req = httptest.NewRequest(http.MethodGet, "/api/secrets", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200", w.Code)
	}
	listBody := w.Body.String()
	if strings.Contains(listBody, "LEAK-FOLLOWUP-MARKER") || strings.Contains(listBody, secretValue) {
		t.Errorf("list response contains secret value after SET\nbody: %s", listBody)
	}
	if !strings.Contains(listBody, "oauth_token") {
		t.Errorf("list response missing expected key\nbody: %s", listBody)
	}
}

// sanity: ensure the Handler serves the secrets routes at all. Guards against
// regressions where the group is mounted under a different path prefix.
func TestAPI_SecretsRoute_Exists(t *testing.T) {
	srv, _ := newSecretsTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/secrets", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code == http.StatusNotFound {
		t.Fatalf("/api/secrets not routed — got 404")
	}
	// Verify content-type is JSON, not HTML.
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("content-type = %q, want application/json", got)
	}
	// Touch the body so the test name still surfaces the method mismatch
	// variant if someone accidentally flips the handler.
	if w.Body.Len() == 0 {
		t.Errorf("empty response body")
	}
}
