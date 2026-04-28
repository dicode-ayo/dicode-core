package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/trigger"
	"go.uber.org/zap"
)

// newHealthServer builds a Server with auth enabled, since the strongest test
// of /healthz is that it bypasses auth.
func newHealthServer(t *testing.T) *Server {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	cfg := &config.Config{
		Server: config.ServerConfig{
			Port:   8080,
			Auth:   true,
			Secret: "test-passphrase",
		},
	}
	srv, err := New(8080, reg, eng, cfg, "", nil, nil, nil, "", NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())
	if err != nil {
		t.Fatalf("webui.New: %v", err)
	}
	return srv
}

func TestHealthzReturns200WithJSON(t *testing.T) {
	srv := newHealthServer(t)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type: got %q, want application/json", ct)
	}

	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status field: got %q, want %q", body["status"], "ok")
	}
	if _, ok := body["version"]; !ok {
		t.Errorf("response missing version field: %v", body)
	}
}

func TestHealthzBypassesAuth(t *testing.T) {
	// With auth enabled, an unauthenticated request to /healthz must succeed.
	// Any 4xx/5xx (especially 401/302-to-login) means the route is gated.
	srv := newHealthServer(t)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("auth-enabled /healthz: got %d, want 200 (route must bypass auth)", w.Code)
	}
}

func TestHealthzReportsConfiguredVersion(t *testing.T) {
	prev := Version
	Version = "v9.9.9-test"
	t.Cleanup(func() { Version = prev })

	srv := newHealthServer(t)
	h := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	var body map[string]string
	_ = json.NewDecoder(w.Body).Decode(&body)
	if body["version"] != "v9.9.9-test" {
		t.Errorf("version: got %q, want %q", body["version"], "v9.9.9-test")
	}
}
