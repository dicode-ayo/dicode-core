package webui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/schemavalidate"
	"github.com/dicode/dicode/pkg/trigger"
	"github.com/go-chi/chi/v5"
)

// Resumer spawns the continuation run for a suspended run. The token is
// resolved server-side from the run record — never supplied by the client —
// so the continuation task is determined by stored state, not the request.
// *trigger.Engine satisfies this via ResumeRun.
type Resumer interface {
	ResumeRun(ctx context.Context, token string, input []byte) (newRunID string, err error)
}

// SetResumer wires a Resumer for the POST /api/runs/{runID}/resume endpoint.
// Pass nil to disable (the endpoint returns 503).
func (s *Server) SetResumer(r Resumer) { s.resumer = r }

// apiResumeRun submits a suspended run's input and spawns its continuation.
// The session (or API key) is the authorization: the raw resume_token is
// resolved from the stored run, never trusted from the client.
//
// The submission is validated against the run's stored JSON Schema before the
// continuation is spawned, so the resumed task can trust ctx.input.
//
// Status codes:
//   - 200 — resumed; body returns the continuation run_id.
//   - 400 — malformed body OR the input fails schema validation.
//   - 404 — run not found OR its token no longer resolves.
//   - 409 — run is not suspended (already resumed / finished — single-use guard).
//   - 410 — resume deadline expired.
//   - 500 — internal error.
//   - 503 — resume not available (Resumer not wired).
func (s *Server) apiResumeRun(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")

	if s.resumer == nil {
		jsonErr(w, "resume not available", http.StatusServiceUnavailable)
		return
	}

	run, err := s.registry.GetRun(r.Context(), runID)
	if err != nil {
		jsonErr(w, "run not found: "+runID, http.StatusNotFound)
		return
	}
	if run.Status != registry.StatusSuspended || run.ResumeToken == "" {
		jsonErr(w, "run is not suspended", http.StatusConflict)
		return
	}

	values := map[string]any{}
	if r.Body != nil && r.ContentLength != 0 {
		dec := json.NewDecoder(r.Body)
		// Preserve numbers as json.Number so integers > 2^53 survive the
		// re-marshal below without being coerced through float64.
		dec.UseNumber()
		if err := dec.Decode(&values); err != nil && err != io.EOF {
			jsonErr(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	// Collected values pass through as opaque JSON — the task reads them as
	// ctx.input.
	input, err := json.Marshal(values)
	if err != nil {
		jsonErr(w, "encode input: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := schemavalidate.Validate(run.ResumeSchema, input); err != nil {
		jsonErr(w, err.Error(), http.StatusBadRequest)
		return
	}

	newRunID, err := s.resumer.ResumeRun(r.Context(), run.ResumeToken, input)
	if err != nil {
		switch {
		case errors.Is(err, trigger.ErrResumeTokenNotFound):
			jsonErr(w, "resume token not found", http.StatusNotFound)
		case errors.Is(err, trigger.ErrResumeNotSuspended):
			jsonErr(w, "run already resumed", http.StatusConflict)
		case errors.Is(err, trigger.ErrResumeExpired):
			jsonErr(w, "resume deadline expired", http.StatusGone)
		case errors.Is(err, trigger.ErrResumePending):
			// The task is held pending approval (or otherwise vetoed); the run
			// stays suspended and resumable once it is approved. 423 Locked
			// signals a retryable, not-yet-permitted state.
			jsonErr(w, "task is awaiting approval; resume once it is approved", http.StatusLocked)
		default:
			jsonErr(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	jsonOK(w, map[string]any{"run_id": newRunID})
}
