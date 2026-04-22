package onboarding

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testBrowserHome = "/home/wizard"

func postJSON(t *testing.T, handler http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestBuildWizardHandler_GetIndex_ServesHTML(t *testing.T) {
	h, _ := buildWizardHandler(testBrowserHome)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET / status = %d; want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q; want text/html...", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(strings.ToLower(body), "dicode") {
		t.Errorf("index body missing 'dicode': %q", body[:min(200, len(body))])
	}
}

func TestBuildWizardHandler_PostApply_FillsResult(t *testing.T) {
	h, resCh := buildWizardHandler(testBrowserHome)

	payload := map[string]any{
		"tasksets": map[string]bool{
			"buildin":  true,
			"examples": false,
			"auth":     true,
		},
		"local_tasks_dir": "/opt/tasks",
		"data_dir":        "/var/dicode",
		"port":            9091,
	}
	rec := postJSON(t, h, "/setup/apply", payload)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /setup/apply status = %d; want 200. body=%q", rec.Code, rec.Body.String())
	}

	var resp struct {
		Passphrase string `json:"passphrase"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%q", err, rec.Body.String())
	}
	if len(resp.Passphrase) != 24 {
		t.Errorf("passphrase len = %d; want 24", len(resp.Passphrase))
	}

	select {
	case got := <-resCh:
		if !got.TaskSetsEnabled["buildin"] || got.TaskSetsEnabled["examples"] || !got.TaskSetsEnabled["auth"] {
			t.Errorf("taskset selection lost: %+v", got.TaskSetsEnabled)
		}
		if got.LocalTasksDir != "/opt/tasks" {
			t.Errorf("LocalTasksDir = %q; want /opt/tasks", got.LocalTasksDir)
		}
		if got.DataDir != "/var/dicode" {
			t.Errorf("DataDir = %q; want /var/dicode", got.DataDir)
		}
		if got.Port != 9091 {
			t.Errorf("Port = %d; want 9091", got.Port)
		}
		if got.Passphrase != resp.Passphrase {
			t.Errorf("channel passphrase %q != response passphrase %q", got.Passphrase, resp.Passphrase)
		}
	case <-time.After(time.Second):
		t.Fatal("no Result on channel after successful /setup/apply")
	}
}

func TestBuildWizardHandler_PostApply_RejectsBadJSON(t *testing.T) {
	h, _ := buildWizardHandler(testBrowserHome)
	req := httptest.NewRequest(http.MethodPost, "/setup/apply",
		strings.NewReader(`{not-json`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

func TestRunBrowser_ContextCancel_ShutsDown(t *testing.T) {
	// Cancel immediately: RunBrowser should return promptly with ctx.Err.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		_, _ = RunBrowser(ctx, testBrowserHome)
		close(done)
	}()

	select {
	case <-done:
		// ok
	case <-time.After(2 * time.Second):
		t.Fatal("RunBrowser did not return after context cancel")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
