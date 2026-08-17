package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"

	"github.com/dicode/dicode/pkg/approval"
	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"
)

// ApprovalGate is the narrow surface of the trust-on-change approval gate
// (pkg/approval.Gate) the WebUI uses: pending visibility, plain approval
// (session / API-key callers), and hash-bound approval (token-link
// redemptions). Defined here so tests can wire a fake — same decoupling as
// SecretsManager.
type ApprovalGate interface {
	IsPending(id string) bool
	PendingHash(id string) (string, bool)
	Approve(id string) error
	ApproveIfHash(id, hash string) error
	State(id string) (approval.State, error)
}

// SetApprovalGate wires the approval gate. Call after New and before Start.
// Nil (the default) hides the pending flag and disables the approve
// endpoints.
func (s *Server) SetApprovalGate(g ApprovalGate) { s.approvalGate = g }

// SetApprovalTokenStore wires the store backing the single-use approve-link
// tokens. Call after New and before Start. Nil (the default) disables the
// /approve/{token} flow and MintApproveLink.
func (s *Server) SetApprovalTokenStore(ts *approval.TokenStore) { s.approvalTokens = ts }

// BroadcastApprovalPending emits the "approval:pending" WebSocket event so
// dashboards can surface a newly held task without polling. The hash is
// shortened to its display form — the browser never needs the full hash.
func (s *Server) BroadcastApprovalPending(taskID, hash string) {
	s.ws.Broadcast(WSMsg{
		Type: "approval:pending",
		Data: ApprovalPendingData{TaskID: taskID, Hash: shortHash(hash)},
	})
}

// taskPendingApproval reports whether id is held by the approval gate.
func (s *Server) taskPendingApproval(id string) bool {
	return s.approvalGate != nil && s.approvalGate.IsPending(id)
}

// MintApproveLink mints a single-use, TTL'd approve token for a pending task
// and returns the absolute URL that redeems it. The notification flow embeds
// this URL so an operator can approve without a full session; the token is
// bound to the task's currently observed content hash, so it cannot approve
// a version the operator never saw.
func (s *Server) MintApproveLink(ctx context.Context, taskID string) (string, error) {
	if s.approvalGate == nil || s.approvalTokens == nil {
		return "", errors.New("approval gate not configured")
	}
	hash, ok := s.approvalGate.PendingHash(taskID)
	if !ok {
		return "", fmt.Errorf("task %q is not pending approval", taskID)
	}
	if hash == "" {
		return "", fmt.Errorf("task %q has no computable content hash", taskID)
	}
	token, err := s.approvalTokens.Mint(ctx, taskID, hash)
	if err != nil {
		return "", err
	}
	return s.WebUIBaseURL() + "/approve/" + token, nil
}

// optionalHash separates a "hash" field that is absent from one that is
// present but carries no usable value. A *string cannot: encoding/json leaves
// it nil for both {} and {"hash":null}.
type optionalHash struct {
	present bool
	value   string
}

// UnmarshalJSON records presence for any value the key holds, null included.
func (o *optionalHash) UnmarshalJSON(b []byte) error {
	o.present = true
	if string(b) == "null" {
		return nil
	}
	return json.Unmarshal(b, &o.value)
}

// approveRequest is apiApproveTask's optional JSON body. Hash, when present,
// binds the approval to the exact pending version the caller reviewed (see
// approval.Diff.PendingHash), mirroring the tokenized /approve/{token} path.
//
// The handler keys off presence, not emptiness, so a caller that meant to bind
// but produced no usable value — a falsy pending_hash reaching JSON.stringify
// as "" or null — is rejected instead of degrading to the unconditional-approve
// path. Only an absent key means "no diff to bind to".
type approveRequest struct {
	Hash optionalHash `json:"hash"`
}

// apiApproveTask handles POST /api/tasks/{id}/approve. Auth mirrors the
// replay endpoint: session cookie or non-ephemeral Bearer API key
// (requireSessionOrNonEphemeralAPIKey).
//
// The dashboard always has a diff on screen before offering Approve, so it
// sends back the hash that diff was built from. Between the diff being
// fetched and this request landing, a push can re-pend the task at a newer
// hash — approving without checking would silently arm content the operator
// never reviewed. A hash-carrying request is therefore routed through
// ApproveIfHash and rejected with 409 (stale:true) on mismatch, so the UI
// can refetch and tell the operator the change moved under them. Callers
// with no diff to bind to — `dicode task approve` goes over IPC, not this
// endpoint, but any other API-key caller in the same position — may omit the
// hash and get the prior unconditional-approve behavior.
//
// Status codes:
//   - 200 — approved; triggers armed, hash recorded in dicode.lock.
//   - 400 — malformed JSON body, or a "hash" field that is empty or null.
//   - 404 — no such task.
//   - 409 — task is not pending approval, or (with stale:true) the supplied
//     hash no longer matches what's pending.
//   - 503 — approval gate not wired.
func (s *Server) apiApproveTask(w http.ResponseWriter, r *http.Request) {
	id := taskIDParam(r)
	if s.approvalGate == nil {
		jsonErr(w, "approval gate not available", http.StatusServiceUnavailable)
		return
	}
	if _, ok := s.registry.GetKinded(id); !ok {
		jsonErr(w, "task not found: "+id, http.StatusNotFound)
		return
	}
	var body approveRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		jsonErr(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	var err error
	switch {
	case body.Hash.present && body.Hash.value == "":
		jsonErr(w, "hash must not be empty or null when supplied", http.StatusBadRequest)
		return
	case body.Hash.present:
		err = s.approvalGate.ApproveIfHash(id, body.Hash.value)
	default:
		err = s.approvalGate.Approve(id)
	}
	if err != nil {
		if errors.Is(err, approval.ErrHashMismatch) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "stale": true})
			return
		}
		jsonErr(w, err.Error(), http.StatusConflict)
		return
	}
	s.log.Info("task approved via API", zap.String("task", id))
	s.ws.Broadcast(WSMsg{Type: "tasks:changed"})
	jsonOK(w, map[string]any{"task_id": id, "approved": true})
}

// apiApprovalPendingState handles GET /api/tasks/{id}/pending-state: the
// resolved end state of a pending task — what will run if the operator arms
// it. Auth mirrors apiApproveTask (same route group).
//
// Status codes:
//   - 200 — the State body.
//   - 404 — no such task.
//   - 409 — task is not pending approval.
//   - 503 — approval gate not wired.
func (s *Server) apiApprovalPendingState(w http.ResponseWriter, r *http.Request) {
	id := taskIDParam(r)
	if s.approvalGate == nil {
		jsonErr(w, "approval gate not available", http.StatusServiceUnavailable)
		return
	}
	if _, ok := s.registry.GetKinded(id); !ok {
		jsonErr(w, "task not found: "+id, http.StatusNotFound)
		return
	}
	state, err := s.approvalGate.State(id)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusConflict)
		return
	}
	jsonOK(w, state)
}

// approvePageTmpl renders the token-link confirm / result pages. Bare HTML on
// purpose: the page must work from a notification click with no session, no
// app shell, and no JS.
var approvePageTmpl = template.Must(template.New("approve").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>dicode — task approval</title>
<meta name="robots" content="noindex">
<style>
body{font-family:system-ui,sans-serif;background:#14151a;color:#e6e6e6;display:flex;justify-content:center;padding-top:10vh}
main{max-width:44rem;padding:2rem;background:#1d1f26;border:1px solid #33363f;border-radius:10px}
code{background:#2a2d36;padding:0.15rem 0.4rem;border-radius:4px;word-break:break-all}
button{background:#3fb950;color:#fff;border:none;border-radius:6px;padding:0.6rem 1.4rem;font-size:1rem;cursor:pointer}
.err{color:#f85149}.ok{color:#3fb950}.meta{color:#8c96a3;font-size:0.85rem}
</style></head><body><main>
{{if .Error}}
  <h1 class="err">Approval failed</h1>
  <p>{{.Error}}</p>
  <p class="meta">Approve links are single-use and expire after 24 hours. Approve the task from the dicode dashboard or with <code>dicode task approve &lt;id&gt;</code>.</p>
{{else if .Approved}}
  <h1 class="ok">Task approved</h1>
  <p>Task <code>{{.TaskID}}</code> is approved — its triggers are armed and the content hash is recorded in <code>dicode.lock</code>.</p>
{{else}}
  <h1>Approve task?</h1>
  <p>This will approve task <code>{{.TaskID}}</code> at content hash <code>{{.Hash}}</code> and arm its triggers.</p>
  <p class="meta">Only approve if you reviewed this task change. The link is single-use.</p>
  <form method="post"><button type="submit">Approve task</button></form>
{{end}}
</main></body></html>`))

type approvePageData struct {
	TaskID   string
	Hash     string
	Approved bool
	Error    string
}

func (s *Server) renderApprovePage(w http.ResponseWriter, status int, data approvePageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The token lives in the URL: never let a follow-up navigation leak it.
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = approvePageTmpl.Execute(w, data)
}

// handleApproveLinkPage serves GET /approve/{token}: a confirm page with the
// approve button. The token is validated (Peek) but NOT consumed — mail
// clients and chat unfurlers prefetch GET links, and a prefetch must neither
// approve the task nor burn the token. Invalid and expired tokens render the
// same generic failure, with status hiding nothing an attacker couldn't
// learn by POSTing anyway.
func (s *Server) handleApproveLinkPage(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	if s.approvalGate == nil || s.approvalTokens == nil {
		s.renderApprovePage(w, http.StatusNotFound, approvePageData{Error: "approval links are not enabled on this daemon"})
		return
	}
	info, err := s.approvalTokens.Peek(r.Context(), token)
	if err != nil {
		s.renderApprovePage(w, http.StatusNotFound, approvePageData{Error: "this approval link is invalid, expired, or already used"})
		return
	}
	// The link must only ever approve what it was minted for: if the task is
	// no longer pending at that exact hash, say so up front.
	if hash, ok := s.approvalGate.PendingHash(info.TaskID); !ok || hash != info.Hash {
		s.renderApprovePage(w, http.StatusConflict, approvePageData{Error: "the task is no longer pending at the version this link was issued for"})
		return
	}
	s.renderApprovePage(w, http.StatusOK, approvePageData{TaskID: info.TaskID, Hash: shortHash(info.Hash)})
}

// handleApproveLinkRedeem serves POST /approve/{token}: consumes the token
// (single-use, even on failure) and approves the bound task iff it is still
// pending at the exact hash the token was minted for.
func (s *Server) handleApproveLinkRedeem(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	if s.approvalGate == nil || s.approvalTokens == nil {
		s.renderApprovePage(w, http.StatusNotFound, approvePageData{Error: "approval links are not enabled on this daemon"})
		return
	}
	info, err := s.approvalTokens.Redeem(r.Context(), token)
	if err != nil {
		s.renderApprovePage(w, http.StatusNotFound, approvePageData{Error: "this approval link is invalid, expired, or already used"})
		return
	}
	if err := s.approvalGate.ApproveIfHash(info.TaskID, info.Hash); err != nil {
		s.log.Warn("approve link redemption rejected",
			zap.String("task", info.TaskID), zap.Error(err))
		s.renderApprovePage(w, http.StatusConflict, approvePageData{Error: err.Error()})
		return
	}
	s.log.Info("task approved via approve link", zap.String("task", info.TaskID))
	s.ws.Broadcast(WSMsg{Type: "tasks:changed"})
	s.renderApprovePage(w, http.StatusOK, approvePageData{TaskID: info.TaskID, Approved: true})
}

// shortHash trims a content hash for display.
func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12] + "…"
	}
	return h
}
