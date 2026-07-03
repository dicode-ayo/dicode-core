// This file contains the webhook trigger surface: path registration, HTTP
// dispatch for kind: Task and kind: PipelineTask webhooks, HMAC signature and
// replay verification, and the webhook UI surface (index.html SDK injection,
// static asset serving, and the task error page).

package trigger

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// reservedOAuthCompletePath is the webhook path the relay broker delivers
// encrypted OAuth tokens to. Only buildin/auth-relay is allowed to bind it —
// any other task that claims this path would be a drop-in exfiltration
// sink for decrypted credentials once the built-in chains the data forward.
const reservedOAuthCompletePath = "/hooks/oauth-complete"
const oauthRelayBuiltinID = "buildin/auth-relay"

func (e *Engine) registerWebhook(spec *task.Spec) {
	e.registerWebhookPath(spec.ID, spec.Trigger.Webhook)
}

// registerWebhookPath claims a webhook path for a task ID (kind-agnostic). The
// webhooks map routes incoming requests by path → task ID; the dispatcher
// resolves the kind. Shared by kind: Task (registerWebhook) and kind:
// PipelineTask (registerPipeline).
func (e *Engine) registerWebhookPath(id, path string) {
	if path == reservedOAuthCompletePath && id != oauthRelayBuiltinID {
		e.log.Warn("rejecting task that tries to shadow reserved OAuth delivery path",
			zap.String("task", id),
			zap.String("path", reservedOAuthCompletePath))
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	// Reject duplicate registrations: a task from a watched repo must not be
	// able to silently hijack another task's webhook path (last-writer-wins
	// was the previous behavior). A task re-registering its own path (e.g.
	// during reconciler reload) is allowed.
	if existing, ok := e.webhooks[path]; ok && existing != id {
		e.log.Warn("rejecting duplicate webhook path — already claimed by another task",
			zap.String("path", path),
			zap.String("existing_task", existing),
			zap.String("rejected_task", id))
		return
	}
	e.webhooks[path] = id
}

// decodeWebhookPayload reads and decodes a webhook request body (or GET query
// params) into an input value. It is the shared kernel for both the kind: Task
// (WebhookHandler) and kind: PipelineTask (handlePipelineWebhook) decode paths
// so the two sites cannot drift.
//
// Contract:
//   - GET with query params → input is map[string]interface{}, body is nil,
//     isForm is false.
//   - POST with application/x-www-form-urlencoded → input is
//     map[string]interface{}, body is the raw body bytes (for HMAC), isForm is
//     true. r.Body is replayed via bytes.NewReader(body) before ParseForm so the
//     caller's HMAC verification still covers the actual bytes.
//   - POST with any other content type (typically application/json) → input is
//     whatever json.Unmarshal produces (or nil if the body is empty/invalid),
//     body is the raw bytes for HMAC, isForm is false.
//
// The raw body is always read up to webhookMaxBodyBytes via a LimitReader before
// any other processing — this preserves the existing HMAC ordering: body bytes
// available to the signature verifier regardless of content-type. Single-value
// query / form entries are flattened to string ([]string{"v"} → "v"); multi-
// value entries remain []string so no information is lost.
//
// isForm is the flag the kind: Task site uses to drive its browser-redirect
// response; the pipeline site ignores it.
func decodeWebhookPayload(r *http.Request, limitedBody []byte) (input interface{}, isForm bool) {
	if r.Method == http.MethodGet {
		if q := r.URL.Query(); len(q) > 0 {
			input = flattenValues(q)
		}
		return input, false
	}
	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "application/x-www-form-urlencoded") {
		// Replay the raw bytes back into r.Body so ParseForm can read them.
		r.Body = io.NopCloser(bytes.NewReader(limitedBody))
		if err := r.ParseForm(); err == nil {
			input = flattenValues(r.Form)
			isForm = true
		}
		return input, isForm
	}
	if len(limitedBody) > 0 {
		_ = json.Unmarshal(limitedBody, &input)
	}
	return input, false
}

// flattenValues converts url.Values-shaped data (query params, form fields)
// into the webhook input map: single-value entries flatten to string
// ([]string{"v"} → "v"), multi-value entries remain []string so no
// information is lost.
func flattenValues(vals map[string][]string) map[string]interface{} {
	m := make(map[string]interface{}, len(vals))
	for k, v := range vals {
		if len(v) == 1 {
			m[k] = v[0]
		} else {
			m[k] = v
		}
	}
	return m
}

// readWebhookBody reads the raw request body up to webhookMaxBodyBytes so
// HMAC verification always covers the actual request bytes, regardless of
// content-type. GET requests have no body — returns nil.
func readWebhookBody(r *http.Request) []byte {
	if r.Method == http.MethodGet || r.Body == nil {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, webhookMaxBodyBytes))
	return body
}

// newWebhookContext captures the request metadata handed to the persistence
// layer (content-type-aware redaction of the raw body, Method/Path/Headers/
// Query on the stored PersistedInput). Shared by the kind: Task and kind:
// PipelineTask dispatch paths.
func newWebhookContext(r *http.Request, body []byte) *pkgruntime.WebhookContext {
	return &pkgruntime.WebhookContext{
		Method:      r.Method,
		Path:        r.URL.Path,
		Headers:     r.Header,
		Query:       r.URL.Query(),
		RawBody:     body,
		ContentType: r.Header.Get("Content-Type"),
	}
}

// writeWebhookReplayRejected writes the 409 JSON envelope shared by the
// kind: Task and kind: PipelineTask replay-rejection paths.
func writeWebhookReplayRejected(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_, _ = w.Write([]byte(`{"error":"duplicate webhook (replay)"}`))
}

const (
	// webhookMaxBodyBytes caps the body read for HMAC verification.
	webhookMaxBodyBytes = 5 << 20 // 5 MB
	// webhookTimestampTolerance is the replay-protection window.
	webhookTimestampTolerance = 5 * time.Minute
	// webhookSignatureHeader is the default signature header (GitHub-compatible).
	webhookSignatureHeader = "X-Hub-Signature-256"
	// webhookTimestampHeader carries the Unix timestamp for replay protection.
	webhookTimestampHeader = "X-Dicode-Timestamp"
)

// taskErrorPage is the HTML template for task failures that produce no output.
// Uses the same ansi-to-html library and log styling as the webui run-detail component.
// Printf args: %s = runID, %s = error message (html-escaped), %s = JSON log lines array.
const taskErrorPage = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<style>
  body { font-family: system-ui, sans-serif; padding: 2rem; background: #1e1e2e; color: #cdd6f4; margin: 0; }
  h2 { color: #f38ba8; margin-top: 0; }
  .run-id { font-family: monospace; font-size: .85em; color: #6c7086; margin-bottom: 1.5rem; }
  .error-msg { background: #302030; border-left: 3px solid #f38ba8; padding: 1rem; border-radius: 4px;
               white-space: pre-wrap; font-family: monospace; font-size: .9em; margin-bottom: 1.5rem; }
  h3 { color: #cdd6f4; margin-bottom: .5rem; }
  pre#logs { background: #181825; border-radius: 6px; padding: 1rem; overflow-x: auto;
             font-family: monospace; font-size: .85em; line-height: 1.5; white-space: pre-wrap; }
  pre#logs span { display: block; }
  pre#logs span.error { color: #f38ba8; }
  pre#logs span.warn  { color: #f9e2af; }
  pre#logs span.info  { color: #cdd6f4; }
</style>
</head>
<body>
<h2>Task error</h2>
<div class="run-id">Run %s</div>
<div class="error-msg">%s</div>
<h3>Logs</h3>
<pre id="logs"></pre>
<script id="log-data" type="application/json">%s</script>
<script>
// Inline ANSI-to-HTML — eliminates the esm.sh CDN dependency (supply-chain
// risk + air-gap breakage). Handles SGR reset, bold, and the 8+8 standard/
// bright foreground colors used by the task runtimes.
function ansiToHtml(s) {
  const FG={31:'#f38ba8',32:'#a6e3a1',33:'#f9e2af',34:'#89b4fa',35:'#cba6f7',36:'#89dceb',37:'#cdd6f4',
             91:'#f38ba8',92:'#a6e3a1',93:'#f9e2af',94:'#89b4fa',95:'#cba6f7',96:'#89dceb',97:'#cdd6f4'};
  function esc(t){return t.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');}
  let out='', depth=0;
  for(const tok of s.split(/(\x1b\[\d+(?:;\d+)*m)/)){
    const m=tok.match(/^\x1b\[(\d+(?:;\d+)*)m$/);
    if(!m){out+=esc(tok);continue;}
    for(const c of m[1].split(';')){
      const n=+c;
      if(!n){out+='</span>'.repeat(depth);depth=0;}
      else if(FG[n]){out+='<span style="color:'+FG[n]+'">';depth++;}
      else if(n===1){out+='<span style="font-weight:bold">';depth++;}
    }
  }
  return out+'</span>'.repeat(depth);
}
const logs = JSON.parse(document.getElementById('log-data').textContent);
const pre = document.getElementById('logs');
if (!logs.length) { pre.textContent = '(no logs)'; }
else { pre.innerHTML = logs.map(l => {
  const cls = /error|uncaught|notcapable/i.test(l) ? 'error' : /warn/i.test(l) ? 'warn' : 'info';
  return '<span class="' + cls + '">' + ansiToHtml(l) + '</span>';
}).join(''); }
</script>
</body>
</html>`

// verifyWebhookSignature validates HMAC-SHA256 signature and optional replay
// protection for a webhook request. Returns nil when the request is authentic.
// When no secret is configured on the task the check is skipped (open webhook).
func verifyWebhookSignature(spec *task.Spec, r *http.Request, body []byte) error {
	return verifyWebhookSignatureSecret(spec.Trigger.WebhookSecret, r, body)
}

// verifyWebhookSignatureSecret is the kind-agnostic HMAC verification core,
// shared by kind: Task (verifyWebhookSignature) and kind: PipelineTask webhook
// dispatch. An empty secret means the webhook is unauthenticated (back-compat).
func verifyWebhookSignatureSecret(secret string, r *http.Request, body []byte) error {
	if secret == "" {
		return nil // unauthenticated webhook — allowed for backwards-compat
	}

	// GET requests have no body — HMAC(secret, "") is a constant that doesn't
	// bind to any request-specific data, enabling (a) replay DoS (all GETs with
	// same secret share the same HMAC digest → 2nd request always 409) and
	// (b) signature reuse across different query strings. Reject GET when a
	// secret is configured.
	if r.Method == http.MethodGet {
		return fmt.Errorf("webhook_secret requires POST; GET is not supported for authenticated webhooks")
	}

	// Validate the optional timestamp and capture it for inclusion in the HMAC
	// preimage. When X-Dicode-Timestamp is present the signature must cover
	// "timestamp_str\nbody" rather than just the body — this binds the signature
	// to a specific time window so a captured request cannot be replayed after
	// the 1-hour replay cache expires (the timestamp changes each signed request).
	var tsStr string
	if raw := r.Header.Get(webhookTimestampHeader); raw != "" {
		ts, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid %s header", webhookTimestampHeader)
		}
		age := time.Since(time.Unix(ts, 0))
		if age < 0 {
			age = -age
		}
		if age > webhookTimestampTolerance {
			return fmt.Errorf("webhook timestamp out of tolerance window (%v)", age.Round(time.Second))
		}
		tsStr = raw
	}

	got := r.Header.Get(webhookSignatureHeader)
	if got == "" {
		return fmt.Errorf("missing %s header", webhookSignatureHeader)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	if tsStr != "" {
		// Include timestamp in signed payload: "<ts_unix_str>\n<body>"
		mac.Write([]byte(tsStr))
		mac.Write([]byte("\n"))
	}
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(got), []byte(want)) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}

// checkWebhookReplay returns an error if the webhook body is a replay.
// When the task has no secret (open webhook) or replay_protection is
// explicitly false, this is a no-op.
func (e *Engine) checkWebhookReplay(secret string, replayProtection *bool, body []byte) error {
	if secret == "" {
		return nil
	}
	if replayProtection != nil && !*replayProtection {
		return nil
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	digest := hex.EncodeToString(mac.Sum(nil))
	if e.webhookReplayCache.seen(digest) {
		return fmt.Errorf("duplicate webhook (replay)")
	}
	return nil
}

// handlePipelineWebhook fires a kind: PipelineTask in response to a webhook
// request and writes the parent run ID as JSON. Pipelines run asynchronously
// (multi-stage), so there is no synchronous inline-result mode — callers poll
// the returned runId. assetPath is non-empty only when the request targeted a
// sub-path; pipelines expose no asset surface, so that is a 404.
//
// The request body (or GET query params) is decoded into a trigger payload and
// threaded into stage 0 via RunOptions.Input / RunOptions.Params (#350),
// mirroring the kind: Task webhook decode path exactly.
func (e *Engine) handlePipelineWebhook(w http.ResponseWriter, r *http.Request, pipe *task.PipelineTask, assetPath string) {
	if assetPath != "" {
		http.NotFound(w, r)
		return
	}

	// Read the raw body first so HMAC verification always covers the actual
	// request bytes; then decode via the shared helper.
	body := readWebhookBody(r)
	input, _ := decodeWebhookPayload(r, body) // isForm ignored — pipelines don't redirect

	if err := verifyWebhookSignatureSecret(pipe.Trigger.WebhookSecret, r, body); err != nil {
		e.log.Warn("pipeline webhook signature verification failed",
			zap.String("path", r.URL.Path), zap.String("task", pipe.ID), zap.Error(err))
		http.Error(w, "forbidden: "+err.Error(), http.StatusForbidden)
		return
	}

	// Replay protection: reject duplicate bodies within the nonce cache TTL.
	if err := e.checkWebhookReplay(pipe.Trigger.WebhookSecret, pipe.Trigger.ReplayProtection, body); err != nil {
		e.log.Warn("pipeline webhook replay rejected",
			zap.String("path", r.URL.Path),
			zap.String("task", pipe.ID),
		)
		writeWebhookReplayRejected(w)
		return
	}

	params := flatStringMap(input)
	webhookCtx := newWebhookContext(r, body)

	e.log.Info("pipeline webhook trigger", zap.String("path", r.URL.Path), zap.String("task", pipe.ID))
	// Decouple from the request context so the async pipeline survives the HTTP
	// response (mirrors fireAsync's use of context.Background()).
	runID, err := e.firePipeline(context.Background(), pipe, pkgruntime.RunOptions{
		Input:      input,
		Params:     params,
		WebhookCtx: webhookCtx,
	}, registry.TriggerWebhook)
	if err != nil {
		http.Error(w, "pipeline failed to start: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("X-Run-Id", runID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"runId": runID})
}

// WebhookHandler returns an HTTP handler that dispatches webhook-triggered tasks.
//
// Behaviour by request type:
//   - GET  /{hookPath}            — if the task directory contains index.html, serve
//     it with the dicode client SDK injected; otherwise run the task with query params.
//   - GET  /{hookPath}/{asset}    — serve a static asset (CSS/JS/image) from the task
//     directory, sandboxed so path traversal is impossible.
//   - POST /{hookPath}            — run the task. JSON body or form-encoded body are
//     both accepted. Browser form submissions (Content-Type: form) redirect to the run
//     result page; API callers receive the usual JSON envelope.
func (e *Engine) WebhookHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// Exact match — normal webhook execution path.
		e.mu.Lock()
		taskID, ok := e.webhooks[path]
		var assetPath, matchedHook string
		if !ok {
			// No exact match — the request is for a static asset under some
			// webhook UI. Walk up one path segment at a time doing exact
			// map lookups, so the most-specific parent hook wins. This
			// matters when both `/hooks/ai` and `/hooks/ai/openai` are
			// registered: `/hooks/ai/openai/chat.js` must bind to the
			// preset, not to the buildin. Exact map lookups (rather than
			// iterating e.webhooks with strings.HasPrefix) are also
			// immune to Go's randomised map iteration order.
			for candidate := path; ; {
				idx := strings.LastIndex(candidate, "/")
				if idx <= 0 {
					break
				}
				candidate = candidate[:idx]
				if tid, found := e.webhooks[candidate]; found {
					taskID = tid
					matchedHook = candidate
					assetPath = path[len(candidate)+1:]
					ok = true
					break
				}
			}
		} else {
			matchedHook = path
		}
		e.mu.Unlock()

		if !ok {
			http.NotFound(w, r)
			return
		}

		// kind: PipelineTask webhooks have no task directory, index.html, or
		// assets — fire the runner directly and return the parent run ID. Stage
		// UIs/assets belong to the individual stage tasks, not the pipeline.
		if k, isKinded := e.registry.GetKinded(taskID); isKinded {
			if pipe, isPipe := k.(*task.PipelineTask); isPipe {
				e.handlePipelineWebhook(w, r, pipe, assetPath)
				return
			}
		}

		spec, ok := e.registry.Get(taskID)
		if !ok {
			http.Error(w, "task not found", http.StatusNotFound)
			return
		}

		// Serve a static asset from the task directory (CSS, JS, images, …).
		// If the sub-path has no recognised file extension and the task has an
		// index.html, fall back to serving that — enabling SPA client-side routing
		// (e.g. /hooks/webui/config, /hooks/webui/tasks/foo all return the SPA shell).
		// This applies to any webhook task that ships an index.html,
		// not just the built-in webui — it is the standard "SPA shell" pattern.
		if assetPath != "" {
			// Block path traversal before any extension check; the SPA fallback
			// must not silently swallow traversal attempts by serving index.html.
			if strings.Contains(assetPath, "..") {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if r.Method == http.MethodGet &&
				filepath.Ext(assetPath) == "" {
				indexFile := filepath.Join(spec.TaskDir, "index.html")
				if data, err := os.ReadFile(indexFile); err == nil {
					html := injectDicodeSDK(string(data), matchedHook, taskID, r)
					w.Header().Set("Content-Type", "text/html; charset=utf-8")
					_, _ = w.Write([]byte(html))
					return
				}
			}
			e.serveTaskAsset(w, r, spec.TaskDir, assetPath)
			return
		}

		// On GET, serve the task's index.html UI when one is present.
		if r.Method == http.MethodGet {
			indexFile := filepath.Join(spec.TaskDir, "index.html")
			if data, err := os.ReadFile(indexFile); err == nil {
				e.log.Info("webhook UI served", zap.String("path", path), zap.String("task", taskID))
				html := injectDicodeSDK(string(data), matchedHook, taskID, r)
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = w.Write([]byte(html))
				return
			}
		}

		e.log.Info("webhook trigger", zap.String("path", path), zap.String("task", taskID))

		// Read the raw body first so HMAC verification always covers the
		// actual request bytes, regardless of content-type.
		body := readWebhookBody(r)
		input, isFormSubmit := decodeWebhookPayload(r, body)

		// Verify HMAC signature when a secret is configured on the task.
		if err := verifyWebhookSignature(spec, r, body); err != nil {
			e.log.Warn("webhook signature verification failed",
				zap.String("path", path),
				zap.String("task", taskID),
				zap.Error(err),
			)
			http.Error(w, "forbidden: "+err.Error(), http.StatusForbidden)
			return
		}

		// Replay protection: reject duplicate bodies within the nonce cache TTL.
		if err := e.checkWebhookReplay(spec.Trigger.WebhookSecret, spec.Trigger.ReplayProtection, body); err != nil {
			e.log.Warn("webhook replay rejected",
				zap.String("path", path),
				zap.String("task", taskID),
			)
			writeWebhookReplayRejected(w)
			return
		}

		// Extract a flat string map from the input so it is accessible via
		// params.get() in task scripts (RunOptions.Params), in addition to the
		// raw input being available as the `input` global (RunOptions.Input).
		params := flatStringMap(input)

		// Build the WebhookContext so the persistence layer can apply
		// content-type-aware redaction to the raw body and populate
		// Method/Path/Headers/Query on the stored PersistedInput.
		// For GET requests body is nil; body was already read above for
		// POST/PUT/etc. and is safe to reference here.
		webhookCtx := newWebhookContext(r, body)

		// Default: wait for the run to finish and return the result inline.
		// Pass ?wait=false to fire-and-forget (returns runId immediately).
		async := r.URL.Query().Get("wait") == "false"

		if async {
			runID, err := e.fireAsync(r.Context(), spec, pkgruntime.RunOptions{Input: input, Params: params, WebhookCtx: webhookCtx}, registry.TriggerWebhook)
			if err != nil {
				http.Error(w, "task failed to start", http.StatusInternalServerError)
				return
			}
			w.Header().Set("X-Run-Id", runID)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"runId": runID})
			return
		}

		// Enforce the concurrency cap at the webhook entry point, not inside
		// fireSync. fireSync is reentrant: if_missing prereqs and input-storage
		// delegation both call fireSync from within runTask, so placing the gate
		// inside fireSync causes a parent holding a slot to self-block when it
		// spawns a nested sub-run. Gate only the top-level webhook-triggered call.
		if e.taskSem != nil && !spec.Trigger.Daemon {
			select {
			case e.taskSem <- struct{}{}:
				defer func() { <-e.taskSem }()
			default:
				http.Error(w, "too many concurrent tasks; retry later", http.StatusServiceUnavailable)
				return
			}
		}

		runID, result, err := e.fireSync(context.Background(), spec, pkgruntime.RunOptions{Input: input, Params: params, WebhookCtx: webhookCtx}, registry.TriggerWebhook)
		if err != nil {
			http.Error(w, "task failed to start", http.StatusInternalServerError)
			return
		}

		// Browser form submissions redirect to the run result page.
		if isFormSubmit {
			http.Redirect(w, r, "/runs/"+runID+"/result", http.StatusSeeOther)
			return
		}

		// Return structured output or return value directly when available.
		if result.OutputContent != "" {
			ct := result.OutputContentType
			if ct == "" {
				ct = "text/plain"
			}
			w.Header().Set("Content-Type", ct+"; charset=utf-8")
			w.Header().Set("X-Run-Id", runID)
			if result.Error != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
			_, _ = w.Write([]byte(result.OutputContent))
			return
		}
		if result.ReturnValue != nil {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Run-Id", runID)
			if result.Error != nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
			_ = json.NewEncoder(w).Encode(result.ReturnValue)
			return
		}

		// No output produced — the task either succeeded silently or threw before
		// calling output.*. Collect logs so we can surface them to the caller.
		var logLines []string
		if logEntries, logErr := e.registry.GetRunLogs(context.Background(), runID); logErr == nil {
			for _, le := range logEntries {
				logLines = append(logLines, le.Message)
			}
		}

		if result.Error != nil {
			errMsg := result.Error.Error()
			// Browser: render an error page using the same log style as the webui.
			if strings.Contains(r.Header.Get("Accept"), "text/html") {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.Header().Set("X-Run-Id", runID)
				w.WriteHeader(http.StatusInternalServerError)
				logsJSON, _ := json.Marshal(logLines)
				var safeJSON bytes.Buffer
				json.HTMLEscape(&safeJSON, logsJSON)
				_, _ = fmt.Fprintf(w, taskErrorPage, html.EscapeString(runID), html.EscapeString(errMsg), safeJSON.String())
				return
			}
			// API: JSON envelope with error message.
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Run-Id", runID)
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"runId":  runID,
				"status": "failure",
				"error":  errMsg,
				"logs":   logLines,
			})
			return
		}

		// Successful run with no output.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Run-Id", runID)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"runId":  runID,
			"status": "success",
			"logs":   logLines,
		})
	})
}

// flatStringMap converts a map[string]interface{} into a map[string]string by
// formatting each value with %v. Returns nil if input is not a flat map.
func flatStringMap(v interface{}) map[string]string {
	m, ok := v.(map[string]interface{})
	if !ok || len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		out[k] = fmt.Sprintf("%v", val)
	}
	return out
}

// injectDicodeSDK injects the dicode client SDK script and context meta tags
// into an HTML page's <head>, allowing the page to use window.dicode.
//
// A <base> tag with a trailing slash is also injected so that relative URLs
// in the task's HTML (e.g. href="style.css") resolve to the correct sub-path
// (e.g. /hooks/my-task/style.css) regardless of the page having no trailing
// slash in its URL.
//
// When the request arrives via the relay proxy, the X-Relay-Base header
// provides the relay path prefix (e.g. /u/<uuid>) so that <base href> and
// script sources are adjusted to work through the relay.
// validRelayBaseRe matches only /u/<64-hex-chars> to prevent header injection.
var validRelayBaseRe = regexp.MustCompile(`^/u/[0-9a-f]{64}$`)

func isValidRelayBase(s string) bool {
	return validRelayBaseRe.MatchString(s)
}

func injectDicodeSDK(htmlDoc, hookPath, taskID string, r *http.Request) string {
	relayBase := r.Header.Get("X-Relay-Base")
	// Only accept relay base paths matching /u/<64-hex-chars>.
	if relayBase != "" && !isValidRelayBase(relayBase) {
		relayBase = ""
	}
	basePath := hookPath
	dicodeJSSrc := "/dicode.js"
	if relayBase != "" {
		basePath = relayBase + hookPath
		dicodeJSSrc = relayBase + "/dicode.js"
	}

	// HTML-escape every interpolated value. All three are already
	// constrained upstream — relayBase by the anchored hex regex above,
	// hookPath by the registered-webhook map match, taskID by the registry —
	// but escaping makes the attribute contexts safe by construction rather
	// than by upstream invariant (CodeQL go/reflected-xss). A no-op for all
	// legitimate values.
	escBase := html.EscapeString(basePath)
	injection := `<base href="` + escBase + `/">` +
		`<meta name="dicode-task" content="` + html.EscapeString(taskID) + `">` +
		`<meta name="dicode-hook" content="` + escBase + `">` +
		`<script src="` + html.EscapeString(dicodeJSSrc) + `"></script>`
	// Inject immediately after <head> so <base> precedes every other element
	// (stylesheets, scripts, images) that carries a relative URL.
	if i := strings.Index(htmlDoc, "<head>"); i != -1 {
		after := i + len("<head>")
		return htmlDoc[:after] + "\n" + injection + htmlDoc[after:]
	}
	// Fallback for pages without a <head> tag.
	return injection + "\n" + htmlDoc
}

// allowedAssetTypes maps file extensions to their Content-Type for webhook UI assets.
var allowedAssetTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".js":    "application/javascript; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".ico":   "image/x-icon",
	".woff":  "font/woff",
	".woff2": "font/woff2",
}

// serveTaskAsset serves a static asset file from a webhook task's directory.
// Access is sandboxed: only known file types are served and path traversal is blocked.
func (e *Engine) serveTaskAsset(w http.ResponseWriter, r *http.Request, taskDir, assetPath string) {
	// Block path traversal before filepath.Clean can resolve it.
	if strings.Contains(assetPath, "..") {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	clean := filepath.Clean(assetPath)
	if filepath.IsAbs(clean) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	ct, allowed := allowedAssetTypes[strings.ToLower(filepath.Ext(clean))]
	if !allowed {
		http.Error(w, "file type not allowed", http.StatusForbidden)
		return
	}

	fullPath := filepath.Join(taskDir, clean)
	// Double-check the resolved path is still inside taskDir.
	if !strings.HasPrefix(fullPath, filepath.Clean(taskDir)+string(filepath.Separator)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	data, err := os.ReadFile(fullPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", ct)
	_, _ = w.Write(data)
}
