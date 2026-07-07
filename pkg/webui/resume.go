package webui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/dicode/dicode/pkg/registry"
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

// resumeFormSchema mirrors the FormSchema the task persisted via
// dicode.suspend(); only the pieces needed for server-side required-field
// validation are modelled. Unknown keys are ignored.
type resumeFormSchema struct {
	Fields []resumeFormField `json:"fields"`
}

type resumeFormField struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
}

// apiResumeRun submits a suspended run's form and spawns its continuation.
// The session (or API key) is the authorization: the raw resume_token is
// resolved from the stored run, never trusted from the client.
//
// Status codes:
//   - 200 — resumed; body returns the continuation run_id.
//   - 400 — malformed body OR a required form field is missing.
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
		if err := json.NewDecoder(r.Body).Decode(&values); err != nil && err != io.EOF {
			jsonErr(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	if missing := firstMissingRequiredField(run.ResumeForm, values); missing != "" {
		jsonErr(w, "missing required field: "+missing, http.StatusBadRequest)
		return
	}

	// Collected values pass through as opaque JSON — the task reads them as
	// ctx.resume_input.
	input, err := json.Marshal(values)
	if err != nil {
		jsonErr(w, "encode input: "+err.Error(), http.StatusInternalServerError)
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
		default:
			jsonErr(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	jsonOK(w, map[string]any{"run_id": newRunID})
}

// firstMissingRequiredField returns the name of the first required field in
// the persisted form schema that has no usable value in the submission, or ""
// if all required fields are satisfied. A boolean field counts as satisfied
// once present (false is a valid answer); other types require a non-empty
// value. A malformed/empty schema imposes no requirements.
func firstMissingRequiredField(formJSON []byte, values map[string]any) string {
	if len(formJSON) == 0 {
		return ""
	}
	var schema resumeFormSchema
	if err := json.Unmarshal(formJSON, &schema); err != nil {
		return ""
	}
	for _, f := range schema.Fields {
		if !f.Required {
			continue
		}
		v, ok := values[f.Name]
		if !ok || v == nil {
			return f.Name
		}
		if f.Type == "boolean" {
			continue
		}
		if s, isStr := v.(string); isStr && s == "" {
			return f.Name
		}
	}
	return ""
}
