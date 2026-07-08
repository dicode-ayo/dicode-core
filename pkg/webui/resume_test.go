package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/trigger"
)

// fakeResumer records the token/input it was called with and returns a
// canned result — standing in for the real *trigger.Engine so handler tests
// exercise the endpoint's parsing, validation, and error mapping in isolation.
type fakeResumer struct {
	gotToken string
	gotInput []byte
	calls    int
	newRunID string
	err      error
}

func (f *fakeResumer) ResumeRun(_ context.Context, token string, input []byte) (string, error) {
	f.calls++
	f.gotToken = token
	f.gotInput = input
	return f.newRunID, f.err
}

// seedSuspendedRun inserts a run and marks it suspended with the given JSON
// Schema and token, returning its run ID.
func seedSuspendedRun(t *testing.T, reg *registry.Registry, schema []byte, token string) string {
	t.Helper()
	ctx := context.Background()
	id, err := reg.StartRunWithID(ctx, "run-suspended-1", "task-a", "", "manual", registry.RunKindTask)
	if err != nil {
		t.Fatalf("StartRunWithID: %v", err)
	}
	if err := reg.SuspendRun(ctx, id, []byte(`{"step":"x"}`), schema, token, 1, 0, nil); err != nil {
		t.Fatalf("SuspendRun: %v", err)
	}
	return id
}

var oneRequiredField = []byte(`{"type":"object","title":"Name?","properties":{"project_name":{"type":"string","title":"Name"}},"required":["project_name"]}`)

func TestApiResumeRun_HappyPath(t *testing.T) {
	srv, reg := newTestServer(t)
	fr := &fakeResumer{newRunID: "continuation-run"}
	srv.SetResumer(fr)
	runID := seedSuspendedRun(t, reg, oneRequiredField, "secret-token")

	req := httptest.NewRequest(http.MethodPost, "/api/runs/"+runID+"/resume",
		strings.NewReader(`{"project_name":"foo"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if fr.calls != 1 {
		t.Fatalf("resumer called %d times, want 1", fr.calls)
	}
	if fr.gotToken != "secret-token" {
		t.Errorf("token = %q, want server-resolved 'secret-token'", fr.gotToken)
	}
	var got map[string]any
	if err := json.Unmarshal(fr.gotInput, &got); err != nil {
		t.Fatalf("input not JSON: %v", err)
	}
	if got["project_name"] != "foo" {
		t.Errorf("input project_name = %v, want foo", got["project_name"])
	}
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body["run_id"] != "continuation-run" {
		t.Errorf("run_id = %v, want continuation-run", body["run_id"])
	}
}

func TestApiResumeRun_MissingRequiredField(t *testing.T) {
	srv, reg := newTestServer(t)
	fr := &fakeResumer{newRunID: "x"}
	srv.SetResumer(fr)
	runID := seedSuspendedRun(t, reg, oneRequiredField, "tok")

	req := httptest.NewRequest(http.MethodPost, "/api/runs/"+runID+"/resume", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "project_name") {
		t.Errorf("body should name the missing field; got %s", w.Body.String())
	}
	if fr.calls != 0 {
		t.Errorf("resumer must not be called when validation fails; calls = %d", fr.calls)
	}
}

func TestApiResumeRun_NotSuspended(t *testing.T) {
	srv, reg := newTestServer(t)
	srv.SetResumer(&fakeResumer{})
	ctx := context.Background()
	id, _ := reg.StartRunWithID(ctx, "run-finished", "task-a", "", "manual", registry.RunKindTask)
	_ = reg.FinishRun(ctx, id, registry.StatusSuccess)

	req := httptest.NewRequest(http.MethodPost, "/api/runs/"+id+"/resume", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", w.Code, w.Body.String())
	}
}

func TestApiResumeRun_RunNotFound(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.SetResumer(&fakeResumer{})

	req := httptest.NewRequest(http.MethodPost, "/api/runs/nope/resume", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", w.Code, w.Body.String())
	}
}

// The engine's typed errors map to distinct HTTP codes. Double-submit surfaces
// as ErrResumeNotSuspended (the single-use guard) → 409; an expired deadline →
// 410; a vanished token → 404.
func TestApiResumeRun_EngineErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"double-submit", trigger.ErrResumeNotSuspended, http.StatusConflict},
		{"expired", trigger.ErrResumeExpired, http.StatusGone},
		{"token-gone", trigger.ErrResumeTokenNotFound, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, reg := newTestServer(t)
			srv.SetResumer(&fakeResumer{err: tc.err})
			runID := seedSuspendedRun(t, reg, nil, "tok")

			req := httptest.NewRequest(http.MethodPost, "/api/runs/"+runID+"/resume", strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)

			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d; body = %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

func TestApiResumeRun_503WhenResumerNotConfigured(t *testing.T) {
	srv, reg := newTestServer(t)
	// Resumer is nil by default.
	runID := seedSuspendedRun(t, reg, oneRequiredField, "tok")

	req := httptest.NewRequest(http.MethodPost, "/api/runs/"+runID+"/resume", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", w.Code, w.Body.String())
	}
}

// With server.auth enabled, an unauthenticated resume (no session, no API key)
// must be rejected before reaching the handler.
func TestApiResumeRun_RequiresAuth(t *testing.T) {
	cfg := &config.Config{Server: config.ServerConfig{Port: 8080, Auth: true, BcryptCost: 4}}
	srv, _ := newTestServerWithSourceMgr(t, cfg, "", nil)
	srv.SetResumer(&fakeResumer{})

	req := httptest.NewRequest(http.MethodPost, "/api/runs/whatever/resume", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", w.Code, w.Body.String())
	}
}

// GET /api/runs/{id} must expose the decoded JSON Schema for rendering but never
// leak the resume token (the resume endpoint resolves it server-side).
func TestApiGetRun_SuspendedExposesSchemaNotToken(t *testing.T) {
	srv, reg := newTestServer(t)
	runID := seedSuspendedRun(t, reg, oneRequiredField, "super-secret")

	req := httptest.NewRequest(http.MethodGet, "/api/runs/"+runID, nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	raw := w.Body.String()
	if strings.Contains(raw, "super-secret") {
		t.Errorf("response leaked resume token: %s", raw)
	}
	var body struct {
		ResumeSchema json.RawMessage `json:"resume_schema"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(string(body.ResumeSchema), "project_name") {
		t.Errorf("resume_schema missing schema properties; got %s", body.ResumeSchema)
	}
}
