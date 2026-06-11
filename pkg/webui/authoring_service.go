package webui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dicode/dicode/pkg/ipc"
	"github.com/google/uuid"
)

// Authoring service layer (#288). The REST handlers in authoring.go and the
// control-socket IPC handlers in pkg/ipc both call these methods, so the
// create/edit/save/cancel business logic lives in exactly one place.

// authoringError carries an HTTP-style status alongside the message so the
// REST layer can map it back to a response code while the IPC layer surfaces
// the same wording. The 409 conflict path (single open session per source)
// references #283 and must reach CLI users intact.
type authoringError struct {
	status int
	msg    string
}

func (e *authoringError) Error() string { return e.msg }

func authErr(status int, format string, a ...any) *authoringError {
	return &authoringError{status: status, msg: fmt.Sprintf(format, a...)}
}

// CreateTask scaffolds boilerplate task files into the named source and
// returns the new task id. An empty source defaults to "ai-scratch"; an
// empty name is generated.
func (s *Server) CreateTask(ctx context.Context, name, source string) (ipc.AuthoringCreateResult, error) {
	if source == "" {
		source = "ai-scratch"
	}
	if s.sourceMgr == nil {
		return ipc.AuthoringCreateResult{}, authErr(503, "source manager not available")
	}
	src, ok := s.sourceMgr.Get(source)
	if !ok {
		return ipc.AuthoringCreateResult{}, authErr(404, "source %q not found", source)
	}

	if name == "" {
		name = "new-task-" + uuid.New().String()[:8]
	}
	name = sanitizeTaskName(name)
	if name == "" {
		return ipc.AuthoringCreateResult{}, authErr(400, "invalid task name")
	}

	repoPath := src.RepoPath()
	if repoPath == "" {
		return ipc.AuthoringCreateResult{}, authErr(503, "source has no repo path; is it started?")
	}
	taskDir := filepath.Join(repoPath, name)
	if err := os.MkdirAll(taskDir, 0755); err != nil {
		return ipc.AuthoringCreateResult{}, authErr(500, "create task dir: %v", err)
	}

	taskYAML := fmt.Sprintf(`apiVersion: dicode/v1
kind: Task
name: %s
description: ""
runtime: deno
trigger:
  manual: true
timeout: 30s
`, name)
	if err := os.WriteFile(filepath.Join(taskDir, "task.yaml"), []byte(taskYAML), 0644); err != nil {
		return ipc.AuthoringCreateResult{}, authErr(500, "write task.yaml: %v", err)
	}

	taskJS := `export default async function main() {
  dicode.log.info("Hello from " + dicode.params.task_id);
}
`
	if err := os.WriteFile(filepath.Join(taskDir, "task.js"), []byte(taskJS), 0644); err != nil {
		return ipc.AuthoringCreateResult{}, authErr(500, "write task.js: %v", err)
	}

	return ipc.AuthoringCreateResult{
		TaskID: source + "/" + name,
		Source: source,
		Files:  []string{"task.yaml", "task.js"},
	}, nil
}

// EditTask opens a new AI edit session for taskID, or resumes the session
// identified by sessionID. Exactly one of the two must be supplied. The
// single-open-session-per-source rule (#283) is enforced here: a fresh edit
// for a source that already has an open session returns a 409 conflict.
func (s *Server) EditTask(ctx context.Context, sessionID, taskID string) (ipc.AuthoringEditResult, error) {
	if s.authoringSessions == nil {
		return ipc.AuthoringEditResult{}, authErr(503, "authoring sessions not available")
	}

	if sessionID != "" {
		sess, err := s.authoringSessions.Get(ctx, sessionID)
		if err != nil {
			return ipc.AuthoringEditResult{}, authErr(500, "lookup session: %v", err)
		}
		if sess == nil {
			return ipc.AuthoringEditResult{}, authErr(404, "session not found")
		}
		if sess.ClosedAt != nil {
			return ipc.AuthoringEditResult{}, authErr(409, "session is closed")
		}
		_ = s.authoringSessions.UpdateLastTurn(ctx, sess.ID)
		return ipc.AuthoringEditResult{
			SessionID:   sess.ID,
			SandboxPath: sess.SandboxPath,
			Source:      sess.Source,
			SourceKind:  s.resolveSourceKind(sess.Source),
		}, nil
	}

	if taskID == "" {
		return ipc.AuthoringEditResult{}, authErr(400, "task_id or session_id required")
	}

	source := taskID
	if idx := strings.Index(source, "/"); idx > 0 {
		source = source[:idx]
	}

	// Single-session-per-source (#283): auto-resume an open session for this
	// source rather than rejecting, so the CLI's task-id-keyed edit verb
	// transparently continues the in-flight conversation. A session opened
	// against a different task in the same source surfaces as a conflict.
	existing, err := s.authoringSessions.GetOpenForSource(ctx, source)
	if err != nil {
		return ipc.AuthoringEditResult{}, authErr(500, "check open sessions: %v", err)
	}
	if existing != nil {
		if existing.TaskID != "" && existing.TaskID != taskID {
			return ipc.AuthoringEditResult{}, authErr(409, "source %q already has an open session %s for task %q (#283)", source, existing.ID, existing.TaskID)
		}
		_ = s.authoringSessions.UpdateLastTurn(ctx, existing.ID)
		return ipc.AuthoringEditResult{
			SessionID:   existing.ID,
			SandboxPath: existing.SandboxPath,
			Source:      existing.Source,
			SourceKind:  s.resolveSourceKind(existing.Source),
		}, nil
	}

	sessID := uuid.New().String()
	now := time.Now()
	sess := AuthoringSession{
		ID:         sessID,
		Kind:       "edit",
		Source:     source,
		TaskID:     taskID,
		CreatedAt:  now,
		LastTurnAt: now,
	}
	if err := s.authoringSessions.Create(ctx, sess); err != nil {
		return ipc.AuthoringEditResult{}, authErr(500, "create session: %v", err)
	}

	return ipc.AuthoringEditResult{
		SessionID:   sessID,
		SandboxPath: sess.SandboxPath,
		Source:      source,
		SourceKind:  s.resolveSourceKind(source),
	}, nil
}

// SaveTask applies the session's pending changes and closes it as applied.
func (s *Server) SaveTask(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return authErr(400, "session_id required")
	}
	if s.authoringSessions == nil {
		return authErr(503, "authoring sessions not available")
	}
	sess, err := s.authoringSessions.Get(ctx, sessionID)
	if err != nil {
		return authErr(500, "lookup session: %v", err)
	}
	if sess == nil {
		return authErr(404, "session not found")
	}
	if sess.ClosedAt != nil {
		return authErr(409, "session is already closed")
	}
	if err := s.authoringSessions.Close(ctx, sess.ID, true); err != nil {
		return authErr(500, "close session: %v", err)
	}
	return nil
}

// CancelTask discards the session. It is idempotent: cancelling an
// already-closed session is a no-op success.
func (s *Server) CancelTask(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return authErr(400, "session_id required")
	}
	if s.authoringSessions == nil {
		return authErr(503, "authoring sessions not available")
	}
	sess, err := s.authoringSessions.Get(ctx, sessionID)
	if err != nil {
		return authErr(500, "lookup session: %v", err)
	}
	if sess == nil {
		return authErr(404, "session not found")
	}
	if sess.ClosedAt != nil {
		return nil
	}
	if err := s.authoringSessions.Close(ctx, sess.ID, false); err != nil {
		return authErr(500, "close session: %v", err)
	}
	return nil
}

// WebUIBaseURL returns the scheme://host:port the browser would use to reach
// this daemon's web UI. TLS config flips the scheme; the host defaults to
// localhost since the daemon binds all interfaces and the CLI runs on the
// same machine.
func (s *Server) WebUIBaseURL() string {
	scheme := "http"
	s.cfgMu.RLock()
	if s.cfg != nil && s.cfg.Server.TLSCertFile != "" && s.cfg.Server.TLSKeyFile != "" {
		scheme = "https"
	}
	s.cfgMu.RUnlock()
	return fmt.Sprintf("%s://localhost:%d", scheme, s.port)
}

// statusFromAuthoringError extracts the HTTP status from an authoringError,
// defaulting to 500 for any other error type.
func statusFromAuthoringError(err error) int {
	var ae *authoringError
	if errors.As(err, &ae) {
		return ae.status
	}
	return 500
}

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
