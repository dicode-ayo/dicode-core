package onboarding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	testBrowserHome = "/home/wizard"
	testPIN         = "123456"
)

func noopApply(Result) error { return nil }

// testGate returns a pinGate pre-seeded with testPIN and a generous
// attempt budget so unrelated tests don't accidentally hit lockout.
func testGate() *pinGate { return newPinGate(testPIN, 5) }

func postJSONWithPIN(t *testing.T, handler http.Handler, path, pin string, body any) *httptest.ResponseRecorder {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	if pin != "" {
		req.Header.Set("X-Setup-Pin", pin)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestBuildWizardHandler_GetIndex_ServesHTML(t *testing.T) {
	h, _ := buildWizardHandler(testBrowserHome, testGate(), 0, noopApply)
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
	h, resCh := buildWizardHandler(testBrowserHome, testGate(), 0, noopApply)

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
	rec := postJSONWithPIN(t, h, "/setup/apply", testPIN, payload)

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
	h, _ := buildWizardHandler(testBrowserHome, testGate(), 0, noopApply)
	req := httptest.NewRequest(http.MethodPost, "/setup/apply",
		strings.NewReader(`{not-json`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Setup-Pin", testPIN)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

// TestBuildWizardHandler_PostApply_RequiresPIN guards against local
// attackers that can read /proc/<pid>/cmdline: the PIN lives only on the
// user's controlling terminal, so a process without terminal access
// cannot supply it.
func TestBuildWizardHandler_PostApply_RequiresPIN(t *testing.T) {
	h, resCh := buildWizardHandler(testBrowserHome, testGate(), 0, noopApply)
	payload := map[string]any{
		"tasksets": map[string]bool{"buildin": true},
		"port":     8080,
	}

	// No PIN header → 403.
	noPIN := postJSONWithPIN(t, h, "/setup/apply", "", payload)
	if noPIN.Code != http.StatusForbidden {
		t.Errorf("no-PIN status = %d; want 403", noPIN.Code)
	}

	// Wrong PIN → 403.
	badPIN := postJSONWithPIN(t, h, "/setup/apply", "000000", payload)
	if badPIN.Code != http.StatusForbidden {
		t.Errorf("bad-PIN status = %d; want 403", badPIN.Code)
	}

	// No result pushed to channel from either rejected attempt.
	select {
	case got := <-resCh:
		t.Fatalf("channel received Result despite PIN rejection: %+v", got)
	default:
	}
}

// TestBuildWizardHandler_PostApply_LocksOutAfterMaxAttempts ensures
// brute-force attempts can't enumerate the 6-digit PIN AND that the
// handler distinguishes "wrong PIN" (403) from "locked" (423) so the
// UI can tell the user to restart the daemon instead of retrying
// forever. With maxAttempts=3, attempts 1 and 2 return 403; the 3rd
// wrong attempt trips the lock and returns 423; any subsequent request
// — even with the correct PIN — returns 423.
func TestBuildWizardHandler_PostApply_LocksOutAfterMaxAttempts(t *testing.T) {
	gate := newPinGate(testPIN, 3)
	h, _ := buildWizardHandler(testBrowserHome, gate, 0, noopApply)
	payload := map[string]any{
		"tasksets": map[string]bool{"buildin": true},
		"port":     8080,
	}
	// Attempts 1 and 2: wrong PIN, still under the cap → 403.
	for i := 0; i < 2; i++ {
		rec := postJSONWithPIN(t, h, "/setup/apply", "999999", payload)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("attempt %d: status = %d; want 403", i, rec.Code)
		}
	}
	// Attempt 3: wrong PIN trips the cap → 423.
	rec := postJSONWithPIN(t, h, "/setup/apply", "999999", payload)
	if rec.Code != http.StatusLocked {
		t.Errorf("lockout-triggering attempt status = %d; want 423", rec.Code)
	}
	// Correct PIN post-lockout: still 423.
	rec = postJSONWithPIN(t, h, "/setup/apply", testPIN, payload)
	if rec.Code != http.StatusLocked {
		t.Errorf("correct-PIN-after-lockout status = %d; want 423", rec.Code)
	}
}

// TestBuildWizardHandler_Apply_Failure_Returns500 guards against the
// "user got a passphrase but config never wrote" race.
func TestBuildWizardHandler_Apply_Failure_Returns500(t *testing.T) {
	boom := errors.New("write-config failed")
	failApply := func(Result) error { return boom }
	h, resCh := buildWizardHandler(testBrowserHome, testGate(), 0, failApply)

	payload := map[string]any{
		"tasksets": map[string]bool{"buildin": true},
		"port":     8080,
	}
	rec := postJSONWithPIN(t, h, "/setup/apply", testPIN, payload)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}

	// On apply failure, no result is pushed to the channel — caller must
	// be able to retry or clean up without a stuck receiver.
	select {
	case got := <-resCh:
		t.Fatalf("channel received Result despite apply failure: %+v", got)
	default:
	}
}

func TestRunBrowser_ContextCancel_ShutsDown(t *testing.T) {
	// Cancel immediately: RunBrowser should return promptly with ctx.Err.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		_, _ = RunBrowser(ctx, testBrowserHome, 0, noopApply)
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
