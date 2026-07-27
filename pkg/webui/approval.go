package webui

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"

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
	Diff(id string) (approval.Diff, error)
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

// apiApproveTask handles POST /api/tasks/{id}/approve. Auth mirrors the
// replay endpoint: session cookie or Bearer API key (requireSessionOrAPIKey).
//
// Status codes:
//   - 200 — approved; triggers armed, hash recorded in dicode.lock.
//   - 404 — no such task.
//   - 409 — task is not pending approval (or the approval failed).
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
	if err := s.approvalGate.Approve(id); err != nil {
		jsonErr(w, err.Error(), http.StatusConflict)
		return
	}
	s.log.Info("task approved via API", zap.String("task", id))
	s.ws.Broadcast(WSMsg{Type: "tasks:changed"})
	jsonOK(w, map[string]any{"task_id": id, "approved": true})
}

// apiApprovalDiff handles GET /api/tasks/{id}/pending-diff: the file-level
// diff between a pending task's last-approved content (if cached) and its
// current pending content, so the operator can review what changed before
// approving. Auth mirrors apiApproveTask (same route group).
//
// Status codes:
//   - 200 — the Diff body (may have an empty Files list if nothing changed
//     at the file level, e.g. a dir-less/inline task).
//   - 404 — no such task.
//   - 409 — task is not pending approval.
//   - 503 — approval gate not wired.
func (s *Server) apiApprovalDiff(w http.ResponseWriter, r *http.Request) {
	id := taskIDParam(r)
	if s.approvalGate == nil {
		jsonErr(w, "approval gate not available", http.StatusServiceUnavailable)
		return
	}
	if _, ok := s.registry.GetKinded(id); !ok {
		jsonErr(w, "task not found: "+id, http.StatusNotFound)
		return
	}
	diff, err := s.approvalGate.Diff(id)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusConflict)
		return
	}
	jsonOK(w, diff)
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
.warn-banner{background:rgba(248,81,73,0.12);border:1px solid #f85149;color:#f85149;padding:0.75rem 1rem;border-radius:6px;margin-bottom:1rem;font-size:0.9rem}
.no-baseline{background:rgba(210,153,34,0.12);border:1px solid #d29922;color:#d29922;padding:0.6rem 1rem;border-radius:6px;margin-bottom:1rem;font-size:0.85rem}
.file-diff{margin-bottom:1rem}
.file-header{font-family:monospace;font-weight:600;margin-bottom:0.35rem;display:flex;align-items:center;gap:0.5rem}
.status-badge{font-size:0.7rem;padding:0.05rem 0.4rem;border-radius:3px;background:#2a2d36;color:#8c96a3;text-transform:uppercase}
.sec-badge{font-size:0.7rem;padding:0.05rem 0.4rem;border-radius:3px;background:rgba(248,81,73,0.18);color:#f85149;border:1px solid rgba(248,81,73,0.45)}
pre.diffblock{background:#0d0f13;padding:0.6rem 0.75rem;border-radius:6px;overflow-x:auto;font-size:0.8rem;line-height:1.45;margin:0;white-space:pre}
.diffline{display:block}
.diffline.add{background:rgba(63,185,80,0.15);color:#3fb950}
.diffline.del{background:rgba(248,81,73,0.15);color:#f85149}
.diffline.ctx{color:#8c96a3}
.diffline.note{color:#8c96a3;font-style:italic}
details.diff-toggle{margin-top:1.25rem}
details.diff-toggle summary{cursor:pointer;color:#8c96a3;font-size:0.85rem;margin-bottom:0.75rem}
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

  {{if .Diff}}
  <details class="diff-toggle" open>
    <summary>What changed ({{len .Diff.Files}} file{{if ne (len .Diff.Files) 1}}s{{end}})</summary>
    {{if not .Diff.HasBaseline}}
      <div class="no-baseline">No prior approved version is cached for this task (fresh daemon session) — the files below are shown as new content, not a diff against a known-good baseline.</div>
    {{end}}
    {{if .Diff.Incomplete}}
      <div class="warn-banner">&#9888; <strong>Incomplete diff.</strong> {{.Diff.IncompleteReason}}</div>
    {{end}}
    {{if .Diff.AnySecurityRelevant}}
      <div class="warn-banner">&#9888; This change touches security-relevant fields (permissions, env, triggers, …). Review carefully before approving.</div>
    {{end}}
    {{range .Diff.Files}}
      <div class="file-diff">
        <div class="file-header">
          <span>{{.Path}}</span>
          <span class="status-badge">{{.Status}}</span>
          {{if .SecurityRelevant}}<span class="sec-badge">security-relevant</span>{{end}}
          {{if .ContentHidden}}<span class="sec-badge">change not shown</span>{{end}}
        </div>
        <pre class="diffblock">{{range .Lines}}<span class="diffline {{.Class}}">{{.Text}}</span>{{end}}</pre>
      </div>
    {{end}}
  </details>
  {{end}}
{{end}}
</main></body></html>`))

type approvePageData struct {
	TaskID   string
	Hash     string
	Approved bool
	Error    string
	Diff     *diffPageView
}

// diffPageView is the token-link page's rendering-friendly projection of
// approval.Diff: each file's UnifiedDiff text is pre-split into
// prefix-classified lines so the bare-HTML/no-JS template can color them
// without any client-side logic.
type diffPageView struct {
	HasBaseline         bool
	AnySecurityRelevant bool
	Incomplete          bool
	IncompleteReason    string
	Files               []diffFileView
}

type diffFileView struct {
	Path             string
	Status           string
	SecurityRelevant bool
	ContentHidden    bool
	Lines            []diffLineView
}

type diffLineView struct {
	Class string // "add", "del", "ctx", or "note" (placeholder/uncategorized text)
	Text  string
}

// buildDiffPageView projects an approval.Diff into diffPageView, splitting
// each FileDiff.UnifiedDiff into individually classed lines by its "+ "/"- "/
// "  " prefix (see unifiedDiffText in pkg/approval/diff.go) so the template
// can render add/remove/context lines with distinct colors. A line matching
// none of those prefixes (the snapshotPlaceholder note) is classed "note".
func buildDiffPageView(d approval.Diff) diffPageView {
	view := diffPageView{
		HasBaseline:      d.HasBaseline,
		Incomplete:       d.Incomplete,
		IncompleteReason: d.IncompleteReason,
	}
	for _, f := range d.Files {
		fv := diffFileView{Path: f.Path, Status: f.Status, SecurityRelevant: f.SecurityRelevant, ContentHidden: f.ContentHidden}
		if f.SecurityRelevant {
			view.AnySecurityRelevant = true
		}
		for _, line := range strings.Split(strings.TrimRight(f.UnifiedDiff, "\n"), "\n") {
			if line == "" {
				continue
			}
			switch {
			case strings.HasPrefix(line, "+ "):
				fv.Lines = append(fv.Lines, diffLineView{Class: "add", Text: line[2:]})
			case strings.HasPrefix(line, "- "):
				fv.Lines = append(fv.Lines, diffLineView{Class: "del", Text: line[2:]})
			case strings.HasPrefix(line, "  "):
				fv.Lines = append(fv.Lines, diffLineView{Class: "ctx", Text: line[2:]})
			default:
				fv.Lines = append(fv.Lines, diffLineView{Class: "note", Text: line})
			}
		}
		view.Files = append(view.Files, fv)
	}
	return view
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
	data := approvePageData{TaskID: info.TaskID, Hash: shortHash(info.Hash)}
	if d, err := s.approvalGate.Diff(info.TaskID); err != nil {
		// The diff is a review aid, not the approval boundary — if it can't
		// be built (e.g. a transient snapshot error) the confirm page still
		// renders and approval still works, just without the "what changed"
		// section.
		s.log.Warn("approve link: diff unavailable", zap.String("task", info.TaskID), zap.Error(err))
	} else {
		view := buildDiffPageView(d)
		data.Diff = &view
	}
	s.renderApprovePage(w, http.StatusOK, data)
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
