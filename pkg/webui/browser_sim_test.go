package webui_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/trigger"
	"github.com/dicode/dicode/pkg/webui"
	"go.uber.org/zap"
)

// TestLogin_FormPost_BrowserOriginHeader pins two properties at once:
//
//  1. A same-origin form POST with a matching Origin header is accepted
//     (the Origin-header check in validateOrigin passes when Origin == Host).
//
//  2. The login page's Referrer-Policy is not `no-referrer` — that header
//     causes Chrome to strip the Origin header from form POSTs, which would
//     break the Origin-check CSRF defence. `strict-origin-when-cross-origin`
//     (or any same-origin-preserving policy) is required.
func TestLogin_FormPost_BrowserOriginHeader(t *testing.T) {
	h := newBrowserTestHandler(t, "hunter2")

	getReq := httptest.NewRequestWithContext(context.Background(), "GET", "http://localhost:8765/login", nil)
	getReq.Host = "localhost:8765"
	getW := httptest.NewRecorder()
	h.ServeHTTP(getW, getReq)
	if getW.Code != 200 {
		t.Fatalf("GET /login: expected 200, got %d", getW.Code)
	}

	// Regression guard: Referrer-Policy must NOT be `no-referrer` (Chrome omits
	// the Origin header on form POSTs under that policy, breaking validateOrigin).
	if rp := getW.Header().Get("Referrer-Policy"); rp == "no-referrer" {
		t.Fatalf("Referrer-Policy must not be no-referrer (strips Origin header on POST, breaks Origin CSRF check); got %q", rp)
	}

	form := url.Values{}
	form.Set("password", "hunter2")
	form.Set("next", "/hooks/webui")

	postReq := httptest.NewRequestWithContext(context.Background(), "POST", "http://localhost:8765/api/auth/login", strings.NewReader(form.Encode()))
	postReq.Host = "localhost:8765"
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.Header.Set("Origin", "http://localhost:8765")
	postReq.Header.Set("Referer", "http://localhost:8765/login")

	postW := httptest.NewRecorder()
	h.ServeHTTP(postW, postReq)
	if postW.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d: %s", postW.Code, postW.Body.String())
	}
}

// TestLogin_FormPost_OriginNull verifies that Origin: null is rejected with 403.
// Browsers send this anonymised origin in sandboxed contexts (data URIs, etc.);
// since the same-origin property cannot be established, the request is refused.
func TestLogin_FormPost_OriginNull(t *testing.T) {
	h := newBrowserTestHandler(t, "hunter2")

	form := url.Values{}
	form.Set("password", "hunter2")

	postReq := httptest.NewRequestWithContext(context.Background(), "POST", "http://localhost:8765/api/auth/login", strings.NewReader(form.Encode()))
	postReq.Host = "localhost:8765"
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.Header.Set("Origin", "null")

	postW := httptest.NewRecorder()
	h.ServeHTTP(postW, postReq)
	if postW.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for Origin: null, got %d", postW.Code)
	}
}

func newBrowserTestHandler(t *testing.T, passphrase string) http.Handler {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	cfg := &config.Config{
		Server: config.ServerConfig{Port: 8765, Auth: true, Secret: passphrase},
	}
	reg := registry.New(d)
	eng := trigger.New(reg, nil, zap.NewNop())
	srv, err := webui.New(8765, reg, eng, cfg, "", nil, nil, nil, "", webui.NewLogBroadcaster(), zap.NewNop(), d, ipc.NewGateway())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv.Handler()
}
