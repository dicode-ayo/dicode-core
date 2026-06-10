package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// --- request / response types -----------------------------------------------

type taskCreateReq struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

type taskCreateResp struct {
	TaskID string   `json:"task_id"`
	Source string   `json:"source"`
	Files  []string `json:"files"`
}

type taskEditReq struct {
	SessionID string `json:"session_id"`
	TaskID    string `json:"task_id"`
	Prompt    string `json:"prompt"`
}

type taskEditResp struct {
	SessionID   string `json:"session_id"`
	SandboxPath string `json:"sandbox_path"`
	Source      string `json:"source"`
	SourceKind  string `json:"source_kind"`
}

type taskSaveReq struct {
	SessionID string `json:"session_id"`
}

type taskCancelReq struct {
	SessionID string `json:"session_id"`
}

// --- POST /api/task/create --------------------------------------------------

func (s *Server) apiTaskCreate(w http.ResponseWriter, r *http.Request) {
	var req taskCreateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	// Default source to "ai-scratch".
	source := req.Source
	if source == "" {
		source = "ai-scratch"
	}

	// Ensure source exists.
	if s.sourceMgr == nil {
		jsonErr(w, "source manager not available", http.StatusServiceUnavailable)
		return
	}
	src, ok := s.sourceMgr.Get(source)
	if !ok {
		jsonErr(w, fmt.Sprintf("source %q not found", source), http.StatusNotFound)
		return
	}

	// Generate task name if not provided.
	name := req.Name
	if name == "" {
		name = "new-task-" + uuid.New().String()[:8]
	}

	// Sanitize: lowercase, replace spaces with hyphens, strip non-alnum/hyphen.
	name = sanitizeTaskName(name)
	if name == "" {
		jsonErr(w, "invalid task name", http.StatusBadRequest)
		return
	}

	// Determine the directory to write the boilerplate.
	repoPath := src.RepoPath()
	if repoPath == "" {
		jsonErr(w, "source has no repo path; is it started?", http.StatusServiceUnavailable)
		return
	}
	taskDir := filepath.Join(repoPath, name)
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		jsonErr(w, fmt.Sprintf("create task dir: %v", err), http.StatusInternalServerError)
		return
	}

	// Write boilerplate task.yaml.
	taskYAML := fmt.Sprintf(`apiVersion: dicode/v1
kind: Task
name: %s
description: ""
runtime: deno
trigger:
  manual: true
timeout: 30s
`, name)
	yamlPath := filepath.Join(taskDir, "task.yaml")
	if err := os.WriteFile(yamlPath, []byte(taskYAML), 0644); err != nil {
		jsonErr(w, fmt.Sprintf("write task.yaml: %v", err), http.StatusInternalServerError)
		return
	}

	// Write boilerplate task.js.
	taskJS := `export default async function main() {
  dicode.log.info("Hello from " + dicode.params.task_id);
}
`
	jsPath := filepath.Join(taskDir, "task.js")
	if err := os.WriteFile(jsPath, []byte(taskJS), 0644); err != nil {
		jsonErr(w, fmt.Sprintf("write task.js: %v", err), http.StatusInternalServerError)
		return
	}

	taskID := source + "/" + name
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(taskCreateResp{
		TaskID: taskID,
		Source: source,
		Files:  []string{"task.yaml", "task.js"},
	})
}

// --- POST /api/task/edit ----------------------------------------------------

func (s *Server) apiTaskEdit(w http.ResponseWriter, r *http.Request) {
	var req taskEditReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if s.authoringSessions == nil {
		jsonErr(w, "authoring sessions not available", http.StatusServiceUnavailable)
		return
	}

	ctx := r.Context()

	// Resume existing session.
	if req.SessionID != "" {
		sess, err := s.authoringSessions.Get(ctx, req.SessionID)
		if err != nil {
			jsonErr(w, fmt.Sprintf("lookup session: %v", err), http.StatusInternalServerError)
			return
		}
		if sess == nil {
			jsonErr(w, "session not found", http.StatusNotFound)
			return
		}
		if sess.ClosedAt != nil {
			jsonErr(w, "session is closed", http.StatusConflict)
			return
		}
		// Bump last_turn_at.
		_ = s.authoringSessions.UpdateLastTurn(ctx, sess.ID)

		sourceKind := s.resolveSourceKind(sess.Source)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(taskEditResp{
			SessionID:   sess.ID,
			SandboxPath: sess.SandboxPath,
			Source:      sess.Source,
			SourceKind:  sourceKind,
		})
		return
	}

	// Need task_id to create/resume.
	if req.TaskID == "" {
		jsonErr(w, "task_id or session_id required", http.StatusBadRequest)
		return
	}

	// Derive source name from task_id (first path segment).
	source := req.TaskID
	if idx := strings.Index(source, "/"); idx > 0 {
		source = source[:idx]
	}

	// Single-session-per-source: check if one is already open.
	existing, err := s.authoringSessions.GetOpenForSource(ctx, source)
	if err != nil {
		jsonErr(w, fmt.Sprintf("check open sessions: %v", err), http.StatusInternalServerError)
		return
	}
	if existing != nil {
		jsonErr(w, fmt.Sprintf("source %q already has an open session %s", source, existing.ID), http.StatusConflict)
		return
	}

	// Create a new session.
	sessID := uuid.New().String()
	now := time.Now()
	sess := AuthoringSession{
		ID:          sessID,
		Kind:        "edit",
		Source:      source,
		TaskID:      req.TaskID,
		SandboxPath: "", // TODO: set when AI agent dispatch is wired
		CreatedAt:   now,
		LastTurnAt:  now,
	}

	if err := s.authoringSessions.Create(ctx, sess); err != nil {
		jsonErr(w, fmt.Sprintf("create session: %v", err), http.StatusInternalServerError)
		return
	}

	// TODO(#288): call set_dev_mode on the source and dispatch to cfg.AI.CreateTask.
	// For this slice the session is created but the AI agent invocation is stubbed.

	sourceKind := s.resolveSourceKind(source)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(taskEditResp{
		SessionID:   sessID,
		SandboxPath: sess.SandboxPath,
		Source:      source,
		SourceKind:  sourceKind,
	})
}

// --- POST /api/task/save ----------------------------------------------------

func (s *Server) apiTaskSave(w http.ResponseWriter, r *http.Request) {
	var req taskSaveReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if req.SessionID == "" {
		jsonErr(w, "session_id required", http.StatusBadRequest)
		return
	}

	if s.authoringSessions == nil {
		jsonErr(w, "authoring sessions not available", http.StatusServiceUnavailable)
		return
	}

	ctx := r.Context()
	sess, err := s.authoringSessions.Get(ctx, req.SessionID)
	if err != nil {
		jsonErr(w, fmt.Sprintf("lookup session: %v", err), http.StatusInternalServerError)
		return
	}
	if sess == nil {
		jsonErr(w, "session not found", http.StatusNotFound)
		return
	}
	if sess.ClosedAt != nil {
		jsonErr(w, "session is already closed", http.StatusConflict)
		return
	}

	// TODO(#288): for git sources, chain into buildin/git-pr.
	// For local sources: rsync sandbox → source root, disable dev mode.

	// Close the session as applied.
	if err := s.authoringSessions.Close(ctx, sess.ID, true); err != nil {
		jsonErr(w, fmt.Sprintf("close session: %v", err), http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{"applied": true})
}

// --- POST /api/task/cancel --------------------------------------------------

func (s *Server) apiTaskCancel(w http.ResponseWriter, r *http.Request) {
	var req taskCancelReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	if req.SessionID == "" {
		jsonErr(w, "session_id required", http.StatusBadRequest)
		return
	}

	if s.authoringSessions == nil {
		jsonErr(w, "authoring sessions not available", http.StatusServiceUnavailable)
		return
	}

	ctx := r.Context()
	sess, err := s.authoringSessions.Get(ctx, req.SessionID)
	if err != nil {
		jsonErr(w, fmt.Sprintf("lookup session: %v", err), http.StatusInternalServerError)
		return
	}
	if sess == nil {
		jsonErr(w, "session not found", http.StatusNotFound)
		return
	}
	if sess.ClosedAt != nil {
		// Already closed — idempotent.
		jsonOK(w, map[string]bool{"cancelled": true})
		return
	}

	// TODO(#288): disable dev mode on the source, clean up sandbox directory.

	if err := s.authoringSessions.Close(ctx, sess.ID, false); err != nil {
		jsonErr(w, fmt.Sprintf("close session: %v", err), http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]bool{"cancelled": true})
}

// --- helpers ----------------------------------------------------------------

// sanitizeTaskName lowercases, replaces spaces with hyphens, and strips
// characters that are not alphanumeric or hyphen.
func sanitizeTaskName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	name = strings.ReplaceAll(name, " ", "-")
	var b strings.Builder
	for _, c := range name {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			b.WriteRune(c)
		}
	}
	return strings.Trim(b.String(), "-")
}

// resolveSourceKind returns "git", "local", or "taskset" for the named source.
func (s *Server) resolveSourceKind(name string) string {
	if s.sourceMgr == nil {
		return ""
	}
	for _, info := range s.sourceMgr.List() {
		if info.Name == name {
			return info.Type
		}
	}
	return ""
}
