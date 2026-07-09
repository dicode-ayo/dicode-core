package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A suspended run has neither OutputContent nor a ReturnValue, so handleRunResult's
// content checks fall through to 404. That is the page a browser lands on after a
// webhook task's form POST suspends (fireWebhookTask redirects form submissions to
// /runs/{id}/result), so the operator saw "not found" instead of the resume form.
func TestRunResult_SuspendedRedirectsToResumeForm(t *testing.T) {
	srv, reg := newTestServer(t)
	ctx := context.Background()

	runID, err := reg.StartRun(ctx, "examples/wizard", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	schema := []byte(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`)
	ok, err := reg.SuspendRun(ctx, runID, []byte(`{}`), schema, "tok-1", 1, 0, nil)
	if err != nil {
		t.Fatalf("SuspendRun: %v", err)
	}
	if !ok {
		t.Fatal("SuspendRun reported no change; the run was not suspended")
	}

	req := httptest.NewRequest(http.MethodGet, "/runs/"+runID+"/result", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want %d (303 to the resume form)", w.Code, http.StatusSeeOther)
	}
	if got, want := w.Header().Get("Location"), "/hooks/webui/runs/"+runID; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
}

// A finished run keeps returning its payload inline — the suspended branch must
// not swallow the normal result path.
func TestRunResult_FinishedStillServesReturnValue(t *testing.T) {
	srv, reg := newTestServer(t)
	ctx := context.Background()

	runID, err := reg.StartRun(ctx, "examples/plain", "")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := reg.FinishRunWithResult(ctx, runID, "success", `{"count":2}`, "", ""); err != nil {
		t.Fatalf("FinishRunWithResult: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/runs/"+runID+"/result", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != `{"count":2}` {
		t.Errorf("body = %q, want the run's return value", got)
	}
}
