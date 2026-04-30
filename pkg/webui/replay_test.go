package webui

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// apiReplayRun's failure paths can be exercised without a fully-wired
// Replayer: when the server's Replayer is nil, the handler should return
// 503 (replay-not-configured). This is the common test path because most
// webui-test fixtures don't wire the InputStore.

func TestApiReplayRun_503WhenReplayerNotConfigured(t *testing.T) {
	srv, _ := newTestServer(t)
	// Replayer is nil by default in newTestServer.

	req := httptest.NewRequest(http.MethodPost, "/api/runs/some-run-id/replay", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "replay") {
		t.Errorf("body should mention 'replay'; got %s", w.Body.String())
	}
}

func TestApiReplayRun_400OnMalformedJSON(t *testing.T) {
	srv, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/runs/some-run-id/replay", strings.NewReader(`not-json`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
}

func TestApiReplayRun_AcceptsTaskNameField(t *testing.T) {
	// A well-formed body with task_name should NOT 400 (decoder accepts the
	// field). Without a wired Replayer it 503s — that's also acceptable here;
	// the assertion is just that body parsing succeeded.
	srv, _ := newTestServer(t)

	body, _ := json.Marshal(map[string]string{"task_name": "task-a"})
	req := httptest.NewRequest(http.MethodPost, "/api/runs/some-run-id/replay", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code == http.StatusBadRequest && strings.Contains(w.Body.String(), "invalid JSON") {
		t.Errorf("body decode failed: %s", w.Body.String())
	}
}

func TestApiReplayRun_RejectsUnknownFields(t *testing.T) {
	srv, _ := newTestServer(t)

	// Unknown top-level field — DisallowUnknownFields should reject.
	req := httptest.NewRequest(http.MethodPost, "/api/runs/some-run-id/replay",
		strings.NewReader(`{"task_name":"x","unknown_field":"y"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
}
