package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// apiAIChat forwards an AI chat request to the task named by cfg.AI.Task.
// It accepts any JSON body — the only required field is `prompt`. The body is
// passed through verbatim to the configured task's webhook so per-task params
// (session_id, skills, tools, model overrides…) travel untouched.
//
// This endpoint intentionally re-enters s.Handler() rather than calling the
// webhook handler directly. The webhook path already layers: longest-prefix
// auth (webhookAuthGuard), asset serving, SPA fallback, IPC gateway dispatch —
// dispatching through the full router keeps the forward identical to what a
// browser would do hitting /hooks/<task> directly. The caller has already
// passed requireAuth (this handler lives inside the authenticated group);
// the forwarded sub-request inherits the session cookie via r.Header.Clone(),
// so when a forwarded-to task has trigger.auth:true the inner
// webhookAuthGuard also passes.
func (s *Server) apiAIChat(w http.ResponseWriter, r *http.Request) {
	// Read the body once so we can replay it on the forwarded request.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		jsonErr(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Minimal schema validation: prompt is required. Anything else is pass-through.
	var probe struct {
		Prompt string `json:"prompt"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &probe); err != nil {
			jsonErr(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	if probe.Prompt == "" {
		jsonErr(w, "prompt is required", http.StatusBadRequest)
		return
	}

	taskID := ""
	if s.cfg != nil {
		taskID = s.cfg.AI.Task
	}
	if taskID == "" {
		jsonErr(w, "ai task not configured or not registered", http.StatusServiceUnavailable)
		return
	}
	spec, ok := s.registry.Get(taskID)
	if !ok {
		s.log.Warn("ai chat: configured task not registered", zap.String("task", taskID))
		jsonErr(w, "ai task not configured or not registered", http.StatusServiceUnavailable)
		return
	}
	if spec.Trigger.Webhook == "" {
		s.log.Warn("ai chat: configured task has no webhook trigger", zap.String("task", taskID))
		jsonErr(w, "configured ai task has no webhook trigger", http.StatusInternalServerError)
		return
	}
	// Guard against self-dispatch recursion and against using the AI
	// endpoint as a proxy to arbitrary /api routes. Webhooks MUST live
	// under /hooks/; anything else points at infrastructure paths the
	// ai-agent runtime cannot legitimately handle.
	if !strings.HasPrefix(spec.Trigger.Webhook, webhookPathPrefix) {
		s.log.Warn("ai chat: configured task webhook is not under "+webhookPathPrefix,
			zap.String("task", taskID), zap.String("webhook", spec.Trigger.Webhook))
		jsonErr(w, "ai task webhook must be under "+webhookPathPrefix, http.StatusInternalServerError)
		return
	}

	// Build the internal forwarded request. Preserve method (POST), headers
	// (cookies, content-type), and body; rewrite the URL to the webhook path.
	//
	// The request's context carries the outer /api/ai/chat chi route context;
	// re-entering the router with it attached confuses chi's internal
	// RouteContext accounting (observed as a spurious 405). Replace it with
	// a fresh chi.Context so the inner router starts from a clean slate
	// while still honouring the parent request's cancellation.
	innerCtx := chi.NewRouteContext()
	ctx := context.WithValue(r.Context(), chi.RouteCtxKey, innerCtx)
	fwdReq, err := http.NewRequestWithContext(ctx, http.MethodPost, spec.Trigger.Webhook, bytes.NewReader(body))
	if err != nil {
		jsonErr(w, "build forward: "+err.Error(), http.StatusInternalServerError)
		return
	}
	fwdReq.Header = r.Header.Clone()
	// Content-Length from the outer request no longer matches — the body is
	// re-wrapped in a bytes.Reader and net/http will recompute it. Stripping
	// avoids confusing any future out-of-process proxy that might honour it.
	fwdReq.Header.Del("Content-Length")
	if fwdReq.Header.Get("Content-Type") == "" {
		fwdReq.Header.Set("Content-Type", "application/json")
	}
	// Propagate client address for downstream IP-aware middleware.
	fwdReq.RemoteAddr = r.RemoteAddr

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, fwdReq)

	// A 404 from the inner router means the webhook path resolved to a
	// registered spec but nothing in the gateway claimed it — typically
	// reconciler lag between registry.Register and gateway wiring at
	// startup. Surface that as a retry-after-style 503 so the UI can
	// show a targeted "try again" rather than a generic "not found".
	//
	// Caveat: today the task runtime has no way to emit a 404 of its own
	// (tasks return JSON reply payloads via output.*, not HTTP statuses).
	// If a future output.status() global lands, this blanket remap will
	// mask legitimate user-emitted 404s — switch to an explicit probe
	// (e.g. presence of a run-id header the task runner injects) at that
	// point.
	if rec.Code == http.StatusNotFound {
		s.log.Warn("ai chat: forward returned 404 — webhook registered but gateway not wired yet",
			zap.String("task", taskID), zap.String("webhook", spec.Trigger.Webhook))
		jsonErr(w, "ai task webhook not yet wired — retry in a moment", http.StatusServiceUnavailable)
		return
	}

	// Propagate the inner response. Content-Type defaults to JSON since the
	// ai-agent buildin always returns JSON; copy the recorded one when set.
	h := w.Header()
	if ct := rec.Header().Get("Content-Type"); ct != "" {
		h.Set("Content-Type", ct)
	} else {
		h.Set("Content-Type", "application/json")
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}
