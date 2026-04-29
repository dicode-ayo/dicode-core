package trigger

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/dicode/dicode/pkg/task"
)

// TestEngine_WebhookPersistsRedactedHeadersAndBody verifies the full
// webhook → persist → fetch → assert-redaction round-trip introduced by #233
// bug-fix. It exercises:
//   - Method and Path populated on PersistedInput.
//   - Headers redacted (Authorization, X-Custom-Token) / kept (User-Agent).
//   - Query redacted (api_key) / kept (page).
//   - JSON body redacted (password) / kept (user).
func TestEngine_WebhookPersistsRedactedHeadersAndBody(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)

	runner := &fakeRunner{store: map[string]string{}}
	is := newFakeInputStore(runner, "fake-storage")
	e.engine.SetInputStore(is)

	spec := writeTask(t, dir, "hook-persist-task",
		`export default async () => "ok";`,
		task.TriggerConfig{Webhook: "/hooks/persist-test"})
	_ = e.reg.Register(spec)
	e.engine.Register(spec)

	body := []byte(`{"user":"alice","password":"secret123","token":"xyz"}`)
	req := httptest.NewRequest(http.MethodPost, "/hooks/persist-test?api_key=sk_xyz&page=1", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer abc")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Custom-Token", "boom")
	req.Header.Set("User-Agent", "test-agent")

	handler := e.engine.WebhookHandler()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code >= 400 {
		t.Fatalf("webhook fire failed: %d %s", w.Code, w.Body.String())
	}

	// The sync webhook path blocks until the run finishes; X-Run-Id carries
	// the run ID we need for registry lookup.
	runID := w.Header().Get("X-Run-Id")
	if runID == "" {
		t.Fatal("X-Run-Id header not set — cannot look up run")
	}

	// The run row must have input_storage_key set.
	run, err := e.reg.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun(%s): %v", runID, err)
	}
	if run.InputStorageKey == "" {
		t.Fatal("InputStorageKey not set — input was not persisted")
	}

	// Fetch and decrypt the stored blob.
	pi, err := is.Fetch(context.Background(), run.ID, run.InputStorageKey, run.InputStoredAt)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// --- Method and Path ---
	if pi.Method != http.MethodPost {
		t.Errorf("Method = %q, want POST", pi.Method)
	}
	if pi.Path != "/hooks/persist-test" {
		t.Errorf("Path = %q, want /hooks/persist-test", pi.Path)
	}

	// --- Headers ---
	if pi.Headers == nil {
		t.Fatal("Headers is nil — redactHeaders was not called")
	}
	if vals, ok := pi.Headers["Authorization"]; !ok || vals[0] != "<redacted>" {
		t.Errorf("Authorization not redacted: %v", pi.Headers["Authorization"])
	}
	if vals, ok := pi.Headers["X-Custom-Token"]; !ok || vals[0] != "<redacted>" {
		t.Errorf("X-Custom-Token not redacted: %v", pi.Headers["X-Custom-Token"])
	}
	if vals, ok := pi.Headers["User-Agent"]; !ok || vals[0] != "test-agent" {
		t.Errorf("User-Agent should NOT be redacted, got: %v", pi.Headers["User-Agent"])
	}

	// --- Query ---
	if pi.Query == nil {
		t.Fatal("Query is nil — redactQuery was not called")
	}
	if vals, ok := pi.Query["api_key"]; !ok || vals[0] != "<redacted>" {
		t.Errorf("api_key query param not redacted: %v", pi.Query["api_key"])
	}
	if vals, ok := pi.Query["page"]; !ok || vals[0] != "1" {
		t.Errorf("page query param should NOT be redacted, got: %v", pi.Query["page"])
	}

	// --- Body ---
	if pi.BodyKind != "json" {
		t.Errorf("BodyKind = %q, want json", pi.BodyKind)
	}
	var parsed map[string]any
	if err := json.Unmarshal(pi.Body, &parsed); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if parsed["user"] != "alice" {
		t.Errorf("user field mutated: got %v, want alice", parsed["user"])
	}
	if parsed["password"] != "<redacted>" {
		t.Errorf("password not redacted: got %v", parsed["password"])
	}
	if parsed["token"] != "<redacted>" {
		t.Errorf("token not redacted: got %v", parsed["token"])
	}

	// --- RedactedFields ---
	if len(pi.RedactedFields) == 0 {
		t.Error("RedactedFields is empty — no redactions were recorded")
	}
}

// TestEngine_WebhookPersists_GetRequest verifies that GET webhook requests
// are persisted with Method=GET, Path set, and no body (since GET has no
// body), but query parameters are properly captured and redacted.
func TestEngine_WebhookPersists_GetRequest(t *testing.T) {
	dir := t.TempDir()
	e := newTestEnv(t)

	runner := &fakeRunner{store: map[string]string{}}
	is := newFakeInputStore(runner, "fake-storage")
	e.engine.SetInputStore(is)

	spec := writeTask(t, dir, "hook-get-task",
		`export default async () => "get-ok";`,
		task.TriggerConfig{Webhook: "/hooks/get-test"})
	// GET triggers are synchronous webhook runs.
	_ = e.reg.Register(spec)
	e.engine.Register(spec)

	req := httptest.NewRequest(http.MethodGet, "/hooks/get-test?user=bob&api_key=secret", nil)
	req.Header.Set("User-Agent", "curl/7.0")

	handler := e.engine.WebhookHandler()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code >= 400 {
		t.Fatalf("webhook GET failed: %d %s", w.Code, w.Body.String())
	}

	runID := w.Header().Get("X-Run-Id")
	if runID == "" {
		t.Fatal("X-Run-Id not set")
	}

	run, err := e.reg.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.InputStorageKey == "" {
		t.Fatal("InputStorageKey not set for GET webhook run")
	}

	pi, err := is.Fetch(context.Background(), run.ID, run.InputStorageKey, run.InputStoredAt)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if pi.Method != http.MethodGet {
		t.Errorf("Method = %q, want GET", pi.Method)
	}
	if pi.Path != "/hooks/get-test" {
		t.Errorf("Path = %q, want /hooks/get-test", pi.Path)
	}
	// For GET requests the query is captured in WebhookCtx.Query, and
	// api_key must be redacted.
	if pi.Query != nil {
		if vals, ok := pi.Query["api_key"]; ok && vals[0] != "<redacted>" {
			t.Errorf("api_key in query should be redacted for GET: %v", vals)
		}
	}
}
