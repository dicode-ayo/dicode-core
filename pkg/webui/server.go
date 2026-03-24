// Package webui serves the REST API and HTMX-based dashboard.
package webui

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/trigger"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// SecretsManager is the interface the secrets UI uses — satisfied by *secrets.LocalProvider.
type SecretsManager interface {
	List(ctx context.Context) ([]string, error)
	Set(ctx context.Context, key, value string) error
	Delete(ctx context.Context, key string) error
}

// sessionStore holds in-memory session tokens for the secrets page.
type sessionStore struct {
	mu     sync.Mutex
	tokens map[string]time.Time
}

func newSessionStore() *sessionStore { return &sessionStore{tokens: make(map[string]time.Time)} }

func (s *sessionStore) issue(passphrase string) string {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	mac := hmac.New(sha256.New, []byte(passphrase))
	mac.Write(raw)
	token := hex.EncodeToString(raw) + "." + hex.EncodeToString(mac.Sum(nil))
	s.mu.Lock()
	s.tokens[token] = time.Now().Add(8 * time.Hour)
	s.mu.Unlock()
	return token
}

func (s *sessionStore) valid(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	exp, ok := s.tokens[token]
	s.mu.Unlock()
	return ok && time.Now().Before(exp)
}

func (s *sessionStore) revoke(token string) {
	s.mu.Lock()
	delete(s.tokens, token)
	s.mu.Unlock()
}

//go:embed templates/*.html
var templateFS embed.FS

// Server is the HTTP server for the web UI and REST API.
type Server struct {
	registry    *registry.Registry
	engine      *trigger.Engine
	cfg         *config.Config
	secretsMgr  SecretsManager // nil if local provider not configured
	sessions    *sessionStore
	logs        *LogBroadcaster
	log         *zap.Logger
	port        int
	srv         *http.Server
	baseTmpl    *template.Template // parsed base.html only; cloned per render
	partialTmpl *template.Template // row partials, no base wrapper
}

// New creates a Server.
func New(port int, r *registry.Registry, eng *trigger.Engine, cfg *config.Config, secretsMgr SecretsManager, logs *LogBroadcaster, log *zap.Logger) (*Server, error) {
	funcMap := template.FuncMap{
		"triggerLabel": triggerLabel,
		"fmtTime":      fmtTime,
		"fmtDuration":  fmtDuration,
		"slice":        func(s string, i, j int) string { return s[i:j] },
		"string":       fmt.Sprint,
		"deref":        func(t *time.Time) time.Time { return *t },
		"toJSON": func(v any) (template.JS, error) {
			b, err := json.Marshal(v)
			return template.JS(b), err
		},
		"list": func(items ...string) []string { return items },
	}
	base, err := template.New("base.html").Funcs(funcMap).ParseFS(templateFS, "templates/base.html")
	if err != nil {
		return nil, fmt.Errorf("parse base template: %w", err)
	}
	partials, err := template.New("").Funcs(funcMap).ParseFS(templateFS,
		"templates/*_rows.html",
		"templates/editor.html",
		"templates/trigger_editor.html",
	)
	if err != nil {
		return nil, fmt.Errorf("parse partial templates: %w", err)
	}
	return &Server{
		registry:    r,
		engine:      eng,
		cfg:         cfg,
		secretsMgr:  secretsMgr,
		sessions:    newSessionStore(),
		logs:        logs,
		log:         log,
		port:        port,
		baseTmpl:    base,
		partialTmpl: partials,
	}, nil
}

// Handler returns the HTTP handler (useful for testing without starting a server).
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.Logger)

	// UI routes
	r.Get("/", s.handleTaskList)
	r.Get("/tasks/{id}", s.handleTaskDetail)
	r.Get("/runs/{runID}", s.handleRunDetail)
	r.Get("/config", s.handleConfig)

	// HTMX partial row updates
	r.Get("/ui/tasks/rows", s.uiTaskRows)
	r.Get("/ui/tasks/{id}/runs/rows", s.uiRunRows)
	r.Get("/ui/tasks/{id}/editor", s.uiTaskEditor)
	r.Get("/ui/tasks/{id}/trigger-editor", s.uiTriggerEditor)

	// SSE log stream
	r.Get("/logs/stream", s.handleLogsStream)

	// File editor API (task.js / task.test.js only)
	r.Get("/api/tasks/{id}/files/{filename}", s.apiGetFile)
	r.Post("/api/tasks/{id}/files/{filename}", s.apiSaveFile)
	r.Post("/api/tasks/{id}/trigger", s.apiSaveTrigger)

	// AI chat — streams SSE, writes task files live
	r.Post("/api/tasks/{id}/ai/stream", s.handleAIStream)

	// REST API
	r.Get("/api/config", s.apiGetConfig)
	r.Get("/api/tasks", s.apiListTasks)
	r.Get("/api/tasks/{id}", s.apiGetTask)
	r.Post("/api/tasks/{id}/run", s.apiRunTask)
	r.Get("/api/tasks/{id}/runs", s.apiListRuns)
	r.Get("/api/runs/{runID}", s.apiGetRun)
	r.Get("/api/runs/{runID}/logs", s.apiGetLogs)

	// Secrets management
	r.Get("/secrets", s.handleSecretsPage)
	r.Post("/secrets/unlock", s.handleSecretsUnlock)
	r.Post("/secrets/lock", s.handleSecretsLock)
	r.Get("/api/secrets", s.apiListSecrets)
	r.Post("/api/secrets", s.apiSetSecret)
	r.Delete("/api/secrets/{key}", s.apiDeleteSecret)

	// Webhook passthrough
	r.Post("/hooks/*", func(w http.ResponseWriter, req *http.Request) {
		req.URL.Path = "/hooks/" + chi.URLParam(req, "*")
		s.engine.WebhookHandler().ServeHTTP(w, req)
	})

	return r
}

// Start listens on the configured port until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	s.srv = &http.Server{
		Addr:    fmt.Sprintf(":%d", s.port),
		Handler: s.Handler(),
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutCtx)
	}()
	s.log.Info("webui listening", zap.Int("port", s.port))
	if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// --- UI handlers ---

func (s *Server) handleTaskList(w http.ResponseWriter, r *http.Request) {
	s.render(w, "tasks.html", map[string]any{
		"Title": "Tasks",
		"Tasks": s.buildTaskRows(r.Context()),
	})
}

func (s *Server) handleTaskDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	spec, ok := s.registry.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	runs, _ := s.registry.ListRuns(r.Context(), id, 20)
	s.render(w, "task.html", map[string]any{
		"Title": spec.Name,
		"Spec":  spec,
		"Runs":  runs,
	})
}

func (s *Server) handleRunDetail(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	run, err := s.registry.GetRun(r.Context(), runID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	logs, _ := s.registry.GetRunLogs(r.Context(), runID)
	s.render(w, "run.html", map[string]any{
		"Title": "Run " + runID[:8],
		"Run":   run,
		"Logs":  logs,
	})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	s.render(w, "config.html", map[string]any{
		"Title": "Config",
		"Cfg":   s.cfg,
	})
}

// --- HTMX partial handlers ---

func (s *Server) uiTaskRows(w http.ResponseWriter, r *http.Request) {
	s.renderPartial(w, "tasks-rows", s.buildTaskRows(r.Context()))
}

func (s *Server) uiRunRows(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	runs, _ := s.registry.ListRuns(r.Context(), id, 20)
	s.renderPartial(w, "runs-rows", runs)
}

// editorData is passed to the editor partial template.
type editorData struct {
	ID         string
	TaskJS     string
	TestJS     string
	TestExists bool
}

func (s *Server) uiTaskEditor(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	spec, ok := s.registry.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	d := editorData{ID: id}
	if b, err := os.ReadFile(filepath.Join(spec.TaskDir, "task.js")); err == nil {
		d.TaskJS = string(b)
	}
	if b, err := os.ReadFile(filepath.Join(spec.TaskDir, "task.test.js")); err == nil {
		d.TestJS = string(b)
		d.TestExists = true
	}
	s.renderPartial(w, "editor", d)
}

// allowedFiles restricts which files the editor API can read/write.
var allowedFiles = map[string]bool{"task.js": true, "task.test.js": true}

func (s *Server) apiGetFile(w http.ResponseWriter, r *http.Request) {
	id, filename := chi.URLParam(r, "id"), chi.URLParam(r, "filename")
	if !allowedFiles[filename] {
		jsonErr(w, "file not allowed", http.StatusBadRequest)
		return
	}
	spec, ok := s.registry.Get(id)
	if !ok {
		jsonErr(w, "task not found", http.StatusNotFound)
		return
	}
	b, err := os.ReadFile(filepath.Join(spec.TaskDir, filename))
	if err != nil {
		jsonErr(w, "file not found", http.StatusNotFound)
		return
	}
	jsonOK(w, map[string]string{"content": string(b)})
}

func (s *Server) apiSaveFile(w http.ResponseWriter, r *http.Request) {
	id, filename := chi.URLParam(r, "id"), chi.URLParam(r, "filename")
	if !allowedFiles[filename] {
		jsonErr(w, "file not allowed", http.StatusBadRequest)
		return
	}
	spec, ok := s.registry.Get(id)
	if !ok {
		jsonErr(w, "task not found", http.StatusNotFound)
		return
	}
	content := r.FormValue("content")
	if err := os.WriteFile(filepath.Join(spec.TaskDir, filename), []byte(content), 0644); err != nil {
		jsonErr(w, "save failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("file saved", zap.String("task", id), zap.String("file", filename))
	jsonOK(w, map[string]string{"status": "saved"})
}

// triggerEditorData is the view model for the trigger editor partial.
type triggerEditorData struct {
	ID          string
	TriggerType string // "cron" | "webhook" | "manual" | "chain"
	Cron        string
	Webhook     string
	ChainFrom   string
	ChainOn     string
}

func triggerEditorDataFromSpec(spec *task.Spec) triggerEditorData {
	d := triggerEditorData{ID: spec.ID}
	switch {
	case spec.Trigger.Cron != "":
		d.TriggerType = "cron"
		d.Cron = spec.Trigger.Cron
	case spec.Trigger.Webhook != "":
		d.TriggerType = "webhook"
		d.Webhook = spec.Trigger.Webhook
	case spec.Trigger.Manual:
		d.TriggerType = "manual"
	case spec.Trigger.Chain != nil:
		d.TriggerType = "chain"
		d.ChainFrom = spec.Trigger.Chain.From
		d.ChainOn = spec.Trigger.Chain.ChainOn()
	}
	return d
}

func (s *Server) uiTriggerEditor(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	spec, ok := s.registry.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.renderPartial(w, "trigger-editor", triggerEditorDataFromSpec(spec))
}

func (s *Server) apiSaveTrigger(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	spec, ok := s.registry.Get(id)
	if !ok {
		jsonErr(w, "task not found", http.StatusNotFound)
		return
	}

	// Read and parse existing task.yaml as a generic map to preserve all other fields.
	yamlPath := filepath.Join(spec.TaskDir, "task.yaml")
	raw, err := os.ReadFile(yamlPath)
	if err != nil {
		jsonErr(w, "read task.yaml: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		jsonErr(w, "parse task.yaml: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Build the new trigger map from form values.
	trigType := r.FormValue("type")
	var trigMap map[string]any
	switch trigType {
	case "cron":
		trigMap = map[string]any{"cron": r.FormValue("cron")}
	case "webhook":
		trigMap = map[string]any{"webhook": r.FormValue("webhook")}
	case "manual":
		trigMap = map[string]any{"manual": true}
	case "chain":
		chain := map[string]any{"from": r.FormValue("chain_from")}
		if on := r.FormValue("chain_on"); on != "" && on != "success" {
			chain["on"] = on
		}
		trigMap = map[string]any{"chain": chain}
	default:
		jsonErr(w, "invalid trigger type", http.StatusBadRequest)
		return
	}

	doc["trigger"] = trigMap
	out, err := yaml.Marshal(doc)
	if err != nil {
		jsonErr(w, "marshal yaml: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.WriteFile(yamlPath, out, 0644); err != nil {
		jsonErr(w, "write task.yaml: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("trigger saved", zap.String("task", id), zap.String("type", trigType))
	jsonOK(w, map[string]string{"status": "saved"})
}

func (s *Server) renderPartial(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.partialTmpl.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("partial render", zap.String("template", name), zap.Error(err))
	}
}

// --- SSE log stream ---

func (s *Server) handleLogsStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	if s.logs == nil {
		http.Error(w, "log streaming not configured", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, recent := s.logs.subscribe()
	defer s.logs.unsubscribe(ch)

	for _, line := range recent {
		fmt.Fprintf(w, "data: %s\n\n", line)
	}
	flusher.Flush()

	for {
		select {
		case line, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", line)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

const secretsCookie = "dicode_secrets_sess"

func (s *Server) secretsPassphrase() string { return s.cfg.Server.Secret }

func (s *Server) secretsSessionValid(r *http.Request) bool {
	// If no passphrase is configured, the session is always valid.
	if s.secretsPassphrase() == "" {
		return true
	}
	c, err := r.Cookie(secretsCookie)
	if err != nil {
		return false
	}
	return s.sessions.valid(c.Value)
}

func (s *Server) handleSecretsPage(w http.ResponseWriter, r *http.Request) {
	if s.secretsMgr == nil {
		s.render(w, "secrets.html", map[string]any{
			"Title":       "Secrets",
			"Unavailable": true,
		})
		return
	}
	unlocked := s.secretsSessionValid(r)
	var keys []string
	if unlocked {
		keys, _ = s.secretsMgr.List(r.Context())
	}
	s.render(w, "secrets.html", map[string]any{
		"Title":     "Secrets",
		"Unlocked":  unlocked,
		"Protected": s.secretsPassphrase() != "",
		"Keys":      keys,
	})
}

func (s *Server) handleSecretsUnlock(w http.ResponseWriter, r *http.Request) {
	entered := r.FormValue("passphrase")
	expected := s.secretsPassphrase()
	if expected == "" || subtle.ConstantTimeCompare([]byte(entered), []byte(expected)) == 1 {
		token := s.sessions.issue(expected + "dicode")
		http.SetCookie(w, &http.Cookie{
			Name:     secretsCookie,
			Value:    token,
			Path:     "/secrets",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			MaxAge:   8 * 3600,
		})
	}
	http.Redirect(w, r, "/secrets", http.StatusSeeOther)
}

func (s *Server) handleSecretsLock(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(secretsCookie); err == nil {
		s.sessions.revoke(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: secretsCookie, Path: "/secrets", MaxAge: -1})
	http.Redirect(w, r, "/secrets", http.StatusSeeOther)
}

func (s *Server) requireSecretsSession(w http.ResponseWriter, r *http.Request) bool {
	if s.secretsMgr == nil {
		jsonErr(w, "secrets not configured", http.StatusServiceUnavailable)
		return false
	}
	if !s.secretsSessionValid(r) {
		jsonErr(w, "secrets locked — unlock at /secrets first", http.StatusUnauthorized)
		return false
	}
	return true
}

func (s *Server) apiListSecrets(w http.ResponseWriter, r *http.Request) {
	if !s.requireSecretsSession(w, r) {
		return
	}
	keys, err := s.secretsMgr.List(r.Context())
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, keys)
}

func (s *Server) apiSetSecret(w http.ResponseWriter, r *http.Request) {
	if !s.requireSecretsSession(w, r) {
		return
	}
	key := r.FormValue("key")
	value := r.FormValue("value")
	if key == "" {
		jsonErr(w, "key is required", http.StatusBadRequest)
		return
	}
	if err := s.secretsMgr.Set(r.Context(), key, value); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("secret set", zap.String("key", key))
	jsonOK(w, map[string]string{"status": "ok"})
}

func (s *Server) apiDeleteSecret(w http.ResponseWriter, r *http.Request) {
	if !s.requireSecretsSession(w, r) {
		return
	}
	key := chi.URLParam(r, "key")
	if err := s.secretsMgr.Delete(r.Context(), key); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("secret deleted", zap.String("key", key))
	jsonOK(w, map[string]string{"status": "ok"})
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	t, err := s.baseTmpl.Clone()
	if err != nil {
		http.Error(w, "template clone error", http.StatusInternalServerError)
		return
	}
	// Include row partials so page templates can call {{template "tasks-rows" ...}} etc.
	if _, err = t.ParseFS(templateFS, "templates/*_rows.html"); err != nil {
		s.log.Error("partial parse", zap.Error(err))
		http.Error(w, "template parse error", http.StatusInternalServerError)
		return
	}
	if _, err = t.ParseFS(templateFS, "templates/"+name); err != nil {
		s.log.Error("template parse", zap.String("template", name), zap.Error(err))
		http.Error(w, "template parse error", http.StatusInternalServerError)
		return
	}
	if err = t.ExecuteTemplate(w, "base.html", data); err != nil {
		s.log.Error("template render", zap.String("template", name), zap.Error(err))
	}
}

// --- REST API handlers ---

func (s *Server) apiGetConfig(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, s.cfg)
}

func (s *Server) apiListTasks(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, s.registry.All())
}

func (s *Server) apiGetTask(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	spec, ok := s.registry.Get(id)
	if !ok {
		jsonErr(w, "task not found", http.StatusNotFound)
		return
	}
	jsonOK(w, spec)
}

func (s *Server) apiRunTask(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	runID, err := s.engine.FireManual(r.Context(), id, nil)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonOK(w, map[string]string{"runId": runID})
}

func (s *Server) apiListRuns(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	runs, err := s.registry.ListRuns(r.Context(), id, 50)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, runs)
}

func (s *Server) apiGetRun(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	run, err := s.registry.GetRun(r.Context(), runID)
	if err != nil {
		jsonErr(w, "run not found", http.StatusNotFound)
		return
	}
	jsonOK(w, run)
}

func (s *Server) apiGetLogs(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	logs, err := s.registry.GetRunLogs(r.Context(), runID)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// For HTMX polling return plain text; for API callers return JSON.
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		w.Header().Set("Content-Type", "text/plain")
		for _, l := range logs {
			fmt.Fprintf(w, "[%s] %s %s\n", l.Level, fmtTime(l.Ts), l.Message)
		}
		return
	}
	jsonOK(w, logs)
}

// --- helpers ---

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// TaskRow pairs a task spec with its most-recent run info for the UI table.
type TaskRow struct {
	*task.Spec
	LastStatus string     // empty if no runs yet
	LastRunID  string     // ID of the most recent run
	LastRunAt  *time.Time // start time of the most recent run
}

// buildTaskRows fetches all tasks and annotates each with last-run info.
func (s *Server) buildTaskRows(ctx context.Context) []TaskRow {
	specs := s.registry.All()
	rows := make([]TaskRow, len(specs))
	for i, spec := range specs {
		row := TaskRow{Spec: spec}
		if runs, err := s.registry.ListRuns(ctx, spec.ID, 1); err == nil && len(runs) > 0 {
			row.LastStatus = runs[0].Status
			row.LastRunID = runs[0].ID
			t := runs[0].StartedAt
			row.LastRunAt = &t
		}
		rows[i] = row
	}
	return rows
}

func triggerLabel(tc task.TriggerConfig) string {
	if tc.Cron != "" {
		return "cron: " + tc.Cron
	}
	if tc.Webhook != "" {
		return "webhook: " + tc.Webhook
	}
	if tc.Chain != nil {
		return "chain: " + tc.Chain.From
	}
	if tc.Manual {
		return "manual"
	}
	return "—"
}

func fmtTime(t time.Time) string {
	return t.Format("2006-01-02 15:04:05")
}

func fmtDuration(start time.Time, end *time.Time) string {
	if end == nil {
		return "running…"
	}
	d := end.Sub(start).Round(time.Millisecond)
	return d.String()
}
