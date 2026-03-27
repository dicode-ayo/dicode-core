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
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	gitSource "github.com/dicode/dicode/pkg/source/git"
	"github.com/dicode/dicode/pkg/source/local"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/trigger"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
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

func (s *sessionStore) purgeLoop() {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for range t.C {
		s.mu.Lock()
		now := time.Now()
		for tok, exp := range s.tokens {
			if now.After(exp) {
				delete(s.tokens, tok)
			}
		}
		s.mu.Unlock()
	}
}

// unlockLimiter is a simple per-IP rate limiter for the secrets unlock endpoint.
// It allows up to maxAttempts requests per window before blocking.
type unlockLimiter struct {
	mu      sync.Mutex
	entries map[string]*limitEntry
}

type limitEntry struct {
	count   int
	resetAt time.Time
}

const (
	unlockMaxAttempts = 5
	unlockWindow      = time.Minute
)

func newUnlockLimiter() *unlockLimiter {
	return &unlockLimiter{entries: make(map[string]*limitEntry)}
}

func (l *unlockLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	e, ok := l.entries[ip]
	if !ok || now.After(e.resetAt) {
		l.entries[ip] = &limitEntry{count: 1, resetAt: now.Add(unlockWindow)}
		return true
	}
	if e.count >= unlockMaxAttempts {
		return false
	}
	e.count++
	return true
}

// securityHeaders adds baseline security headers to every response.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

//go:embed templates/*.html
var templateFS embed.FS

// Server is the HTTP server for the web UI and REST API.
type Server struct {
	registry        *registry.Registry
	engine          *trigger.Engine
	cfg             *config.Config
	cfgPath         string               // path to dicode.yaml; empty in tests
	secretsMgr      SecretsManager       // nil if local provider not configured
	reconciler      *registry.Reconciler // nil if not wired
	dataDir         string               // ~/.dicode or cfg.DataDir
	managedRuntimes []pkgruntime.ManagedRuntime
	sessions        *sessionStore
	limiter         *unlockLimiter
	logs            *LogBroadcaster
	log             *zap.Logger
	port            int
	srv             *http.Server
	baseTmpl        *template.Template // parsed base.html only; cloned per render
	partialTmpl     *template.Template // row partials, no base wrapper
}

// SetManagedRuntimes registers the list of managed runtimes (Deno, Python, …)
// that will appear in the Config UI. Call this after New and before Start.
func (s *Server) SetManagedRuntimes(runtimes []pkgruntime.ManagedRuntime) {
	s.managedRuntimes = runtimes
}

// New creates a Server. cfgPath is the path to dicode.yaml used to persist
// settings changes; pass "" in tests or when persistence is not needed.
// rec and dataDir enable live source management; pass nil/"" in tests.
func New(port int, r *registry.Registry, eng *trigger.Engine, cfg *config.Config, cfgPath string, secretsMgr SecretsManager, rec *registry.Reconciler, dataDir string, logs *LogBroadcaster, log *zap.Logger) (*Server, error) {
	funcMap := template.FuncMap{
		"triggerLabel": triggerLabel,
		"fmtTime":      fmtTime,
		"fmtDuration":  fmtDuration,
		"slice": func(s string, i, j int) string {
			if i < 0 {
				i = 0
			}
			if j > len(s) {
				j = len(s)
			}
			if i > j {
				return ""
			}
			return s[i:j]
		},
		"string": fmt.Sprint,
		"deref":  func(t *time.Time) time.Time { return *t },
		"toJSON": func(v any) (template.JS, error) {
			b, err := json.Marshal(v)
			return template.JS(b), err
		},
		"list":      func(items ...string) []string { return items },
		"not":       func(b bool) bool { return !b },
		"derefBool": func(b *bool) bool { return b != nil && *b },
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
	ss := newSessionStore()
	go ss.purgeLoop()
	return &Server{
		registry:    r,
		engine:      eng,
		cfg:         cfg,
		cfgPath:     cfgPath,
		secretsMgr:  secretsMgr,
		reconciler:  rec,
		dataDir:     dataDir,
		sessions:    ss,
		limiter:     newUnlockLimiter(),
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
	r.Use(securityHeaders)

	// UI routes
	r.Get("/", s.handleTaskList)
	r.Get("/tasks/{id}", s.handleTaskDetail)
	r.Get("/runs/{runID}", s.handleRunDetail)
	r.Get("/config", s.handleConfig)
	r.Get("/config/code", s.handleConfigCode)

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

	// Settings (persist to dicode.yaml)
	r.Post("/api/settings/ai", s.apiSaveAISettings)
	r.Post("/api/settings/server", s.apiSaveServerSettings)
	r.Post("/api/settings/sources", s.apiAddSource)
	r.Delete("/api/settings/sources/{idx}", s.apiRemoveSource)
	r.Get("/api/settings/sources/git/branches", s.apiListGitBranches)

	// Managed runtime lifecycle
	r.Get("/api/runtimes", s.apiListRuntimes)
	r.Post("/api/runtimes/{name}/install", s.apiInstallRuntime)
	r.Delete("/api/runtimes/{name}", s.apiRemoveRuntime)

	// REST API
	r.Get("/api/config", s.apiGetConfig)
	r.Get("/api/config/raw", s.apiGetConfigRaw)
	r.Post("/api/config/raw", s.apiSaveConfigRaw)
	r.Get("/api/tasks", s.apiListTasks)
	r.Get("/api/tasks/{id}", s.apiGetTask)
	r.Post("/api/tasks/{id}/run", s.apiRunTask)
	r.Get("/api/tasks/{id}/runs", s.apiListRuns)
	r.Get("/api/runs/{runID}", s.apiGetRun)
	r.Get("/api/runs/{runID}/logs", s.apiGetLogs)
	r.Post("/api/runs/{runID}/kill", s.apiKillRun)

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
		Addr:              fmt.Sprintf(":%d", s.port),
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,   // mitigates Slowloris
		IdleTimeout:       120 * time.Second, // clean up idle keep-alives
		// WriteTimeout is intentionally 0: SSE and AI stream endpoints write indefinitely.
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
	title := runID
	if len(title) > 8 {
		title = title[:8]
	}
	s.render(w, "run.html", map[string]any{
		"Title": "Run " + title,
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

func (s *Server) handleConfigCode(w http.ResponseWriter, r *http.Request) {
	s.render(w, "config_code.html", map[string]any{
		"Title": "Edit config",
	})
}

// apiGetConfigRaw returns the raw content of dicode.yaml.
// Guarded by secrets session because the config may contain API keys.
func (s *Server) apiGetConfigRaw(w http.ResponseWriter, r *http.Request) {
	if !s.secretsSessionValid(r) {
		jsonErr(w, "unlock secrets first to access raw config", http.StatusUnauthorized)
		return
	}
	if s.cfgPath == "" {
		jsonOK(w, map[string]string{"content": "# config file path not set"})
		return
	}
	b, err := os.ReadFile(s.cfgPath)
	if err != nil {
		s.log.Error("read config file", zap.Error(err))
		jsonErr(w, "could not read config file", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]string{"content": string(b)})
}

// apiSaveConfigRaw validates and writes the raw config back to dicode.yaml,
// then reloads it into memory.
// Guarded by secrets session because the config may contain API keys.
func (s *Server) apiSaveConfigRaw(w http.ResponseWriter, r *http.Request) {
	if !s.secretsSessionValid(r) {
		jsonErr(w, "unlock secrets first to edit raw config", http.StatusUnauthorized)
		return
	}
	if s.cfgPath == "" {
		jsonErr(w, "config file path not set", http.StatusBadRequest)
		return
	}
	content := r.FormValue("content")

	// Validate: must parse as valid YAML mapping.
	var check map[string]any
	if err := yaml.Unmarshal([]byte(content), &check); err != nil {
		jsonErr(w, "invalid YAML: "+err.Error(), http.StatusBadRequest)
		return
	}

	if err := os.WriteFile(s.cfgPath, []byte(content), 0600); err != nil {
		jsonErr(w, "write config: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Hot-reload config into memory (best-effort; server restart needed for port changes).
	if newCfg, err := config.Load(s.cfgPath); err == nil {
		s.cfg = newCfg
	} else {
		s.log.Warn("config reload after raw save failed", zap.Error(err))
	}

	s.log.Info("config saved via code editor")
	jsonOK(w, map[string]string{"status": "ok"})
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
	ScriptFile string // "task.ts" or "task.js"
	TaskJS     string // content of ScriptFile
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
	// Prefer task.ts for deno tasks; fall back to task.js.
	for _, name := range []string{"task.ts", "task.js"} {
		if b, err := os.ReadFile(filepath.Join(spec.TaskDir, name)); err == nil {
			d.ScriptFile = name
			d.TaskJS = string(b)
			break
		}
	}
	if d.ScriptFile == "" {
		d.ScriptFile = "task.ts" // default for new deno tasks
	}
	for _, name := range []string{"task.test.ts", "task.test.js"} {
		if b, err := os.ReadFile(filepath.Join(spec.TaskDir, name)); err == nil {
			d.TestJS = string(b)
			d.TestExists = true
			break
		}
	}
	s.renderPartial(w, "editor", d)
}

// allowedFiles restricts which files the editor API can read/write.
var allowedFiles = map[string]bool{
	"task.js": true, "task.ts": true,
	"task.test.js": true, "task.test.ts": true,
}

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
	if err := os.WriteFile(filepath.Join(spec.TaskDir, filename), []byte(content), 0600); err != nil {
		jsonErr(w, "save failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("file saved", zap.String("task", id), zap.String("file", filename))
	jsonOK(w, map[string]string{"status": "saved"})
}

// triggerEditorData is the view model for the trigger editor partial.
type triggerEditorData struct {
	ID          string
	TriggerType string // "cron" | "webhook" | "manual" | "chain" | "daemon"
	Cron        string
	Webhook     string
	ChainFrom   string
	ChainOn     string
	Restart     string
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
	case spec.Trigger.Daemon:
		d.TriggerType = "daemon"
		d.Restart = spec.Trigger.Restart
		if d.Restart == "" {
			d.Restart = "always"
		}
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
	case "daemon":
		trigMap = map[string]any{"daemon": true}
		if restart := r.FormValue("restart"); restart != "" && restart != "always" {
			trigMap["restart"] = restart
		}
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
	// Rate-limit by client IP: max 5 attempts per minute.
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if !s.limiter.allow(ip) {
		http.Error(w, "too many unlock attempts — try again in a minute", http.StatusTooManyRequests)
		return
	}

	entered := r.FormValue("passphrase")
	expected := s.secretsPassphrase()
	if expected == "" || subtle.ConstantTimeCompare([]byte(entered), []byte(expected)) == 1 {
		token := s.sessions.issue(expected + "dicode")
		http.SetCookie(w, &http.Cookie{
			Name:     secretsCookie,
			Value:    token,
			Path:     "/secrets",
			HttpOnly: true,
			Secure:   true, // must be set; deploy behind TLS or set server.insecure for dev
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
	s.log.Info("run requested via API", zap.String("task", id))
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

	// Parse optional ?since=N cursor for incremental HTMX polling.
	var sinceID int64
	if v := r.URL.Query().Get("since"); v != "" {
		fmt.Sscanf(v, "%d", &sinceID)
	}

	var (
		logs []*registry.LogEntry
		err  error
	)
	if sinceID > 0 {
		logs, err = s.registry.GetRunLogsSince(r.Context(), runID, sinceID)
	} else {
		logs, err = s.registry.GetRunLogs(r.Context(), runID)
	}
	if err != nil {
		s.log.Error("get run logs", zap.String("run", runID), zap.Error(err))
		jsonErr(w, "internal error", http.StatusInternalServerError)
		return
	}

	// HTMX polling: return HTML spans so they can be appended with beforeend.
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		for _, l := range logs {
			// Each line is a <span> carrying the log ID so the client can track the cursor.
			fmt.Fprintf(w, "<span data-log-id=\"%d\">[%s] %s %s\n</span>",
				l.ID, template.HTMLEscapeString(l.Level),
				template.HTMLEscapeString(fmtTime(l.Ts)),
				template.HTMLEscapeString(l.Message),
			)
		}
		return
	}
	jsonOK(w, logs)
}

func (s *Server) apiKillRun(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	s.log.Info("kill requested via API", zap.String("run", runID))
	if !s.engine.KillRun(runID) {
		jsonErr(w, "run not found or already finished", http.StatusNotFound)
		return
	}
	jsonOK(w, map[string]string{"status": "killing"})
}

// --- Settings handlers ---

// apiSaveAISettings updates the AI config section in memory and persists to dicode.yaml.
func (s *Server) apiSaveAISettings(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		jsonErr(w, "bad request", http.StatusBadRequest)
		return
	}
	s.cfg.AI.BaseURL = strings.TrimSpace(r.FormValue("base_url"))
	s.cfg.AI.Model = strings.TrimSpace(r.FormValue("model"))
	s.cfg.AI.APIKeyEnv = strings.TrimSpace(r.FormValue("api_key_env"))
	// Only overwrite APIKey if the user submitted a non-empty value;
	// blank means "keep using env var".
	if v := r.FormValue("api_key"); v != "" {
		s.cfg.AI.APIKey = v
	} else if r.FormValue("clear_api_key") == "1" {
		s.cfg.AI.APIKey = ""
	}

	if err := s.persistConfig(); err != nil {
		s.log.Warn("settings persist failed", zap.Error(err))
		jsonErr(w, "saved in memory but could not write file: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("AI settings updated", zap.String("model", s.cfg.AI.Model), zap.String("base_url", s.cfg.AI.BaseURL))
	jsonOK(w, map[string]string{"status": "ok"})
}

// apiSaveServerSettings updates server.secret and log_level in memory + file.
func (s *Server) apiSaveServerSettings(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		jsonErr(w, "bad request", http.StatusBadRequest)
		return
	}
	if v := r.FormValue("log_level"); v != "" {
		s.cfg.LogLevel = v
	}
	if r.Form.Has("secret") {
		s.cfg.Server.Secret = r.FormValue("secret")
	}
	if v := r.FormValue("tray"); v != "" {
		enabled := v == "true"
		s.cfg.Server.Tray = &enabled
	}

	if err := s.persistConfig(); err != nil {
		s.log.Warn("settings persist failed", zap.Error(err))
		jsonErr(w, "saved in memory but could not write file: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("server settings updated")
	jsonOK(w, map[string]string{"status": "ok"})
}

// apiAddSource adds a new source (local or git) at runtime and persists it.
func (s *Server) apiAddSource(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		jsonErr(w, "bad request", http.StatusBadRequest)
		return
	}
	srcType := r.FormValue("type")
	var sc config.SourceConfig
	switch config.SourceType(srcType) {
	case config.SourceTypeLocal:
		path := strings.TrimSpace(r.FormValue("path"))
		if path == "" {
			jsonErr(w, "path is required for local source", http.StatusBadRequest)
			return
		}
		sc = config.SourceConfig{Type: config.SourceTypeLocal, Path: path, Watch: true}
	case config.SourceTypeGit:
		url := strings.TrimSpace(r.FormValue("url"))
		if url == "" {
			jsonErr(w, "url is required for git source", http.StatusBadRequest)
			return
		}
		sc = config.SourceConfig{
			Type:   config.SourceTypeGit,
			URL:    url,
			Branch: strings.TrimSpace(r.FormValue("branch")),
			Auth: config.SourceAuth{
				Type:     r.FormValue("auth_type"),
				TokenEnv: r.FormValue("token_env"),
			},
		}
		if sc.Branch == "" {
			sc.Branch = "main"
		}
		sc.PollInterval = 30 * 1e9 // 30s in nanoseconds as time.Duration
	default:
		jsonErr(w, "type must be 'local' or 'git'", http.StatusBadRequest)
		return
	}

	// Build and start the source.
	if s.reconciler != nil {
		switch sc.Type {
		case config.SourceTypeLocal:
			ls, err := local.New(sc.Path, sc.Path, s.log)
			if err != nil {
				jsonErr(w, "create local source: "+err.Error(), http.StatusInternalServerError)
				return
			}
			if err := s.reconciler.AddSource(ls); err != nil {
				jsonErr(w, "start source: "+err.Error(), http.StatusInternalServerError)
				return
			}
		case config.SourceTypeGit:
			gs, err := gitSource.New(s.dataDir, sc.URL, sc.Branch, sc.PollInterval, sc.Auth.TokenEnv, sc.Auth.SSHKey, s.log)
			if err != nil {
				jsonErr(w, "create git source: "+err.Error(), http.StatusInternalServerError)
				return
			}
			if err := s.reconciler.AddSource(gs); err != nil {
				jsonErr(w, "start source: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
	}

	// Persist to config.
	s.cfg.Sources = append(s.cfg.Sources, sc)
	if err := s.persistConfig(); err != nil {
		s.log.Warn("source persist failed", zap.Error(err))
	}
	s.log.Info("source added", zap.String("type", srcType))
	jsonOK(w, map[string]string{"status": "ok"})
}

// apiListGitBranches lists branches for a remote git URL.
// GET /api/settings/sources/git/branches?url=...&token_env=...
func (s *Server) apiListGitBranches(w http.ResponseWriter, r *http.Request) {
	repoURL := r.URL.Query().Get("url")
	if repoURL == "" {
		jsonErr(w, "url is required", http.StatusBadRequest)
		return
	}
	tokenEnv := r.URL.Query().Get("token_env")
	branches, err := gitSource.ListBranches(r.Context(), repoURL, tokenEnv)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonOK(w, branches)
}

// apiRemoveSource removes a source by its index in cfg.Sources and stops it.
func (s *Server) apiRemoveSource(w http.ResponseWriter, r *http.Request) {
	idxStr := chi.URLParam(r, "idx")
	idx := 0
	if _, err := fmt.Sscanf(idxStr, "%d", &idx); err != nil || idx < 0 || idx >= len(s.cfg.Sources) {
		jsonErr(w, "invalid index", http.StatusBadRequest)
		return
	}
	sc := s.cfg.Sources[idx]
	if s.reconciler != nil {
		switch sc.Type {
		case config.SourceTypeLocal:
			s.reconciler.RemoveSource(sc.Path)
		case config.SourceTypeGit:
			s.reconciler.RemoveSource(sc.URL)
		}
	}
	s.cfg.Sources = append(s.cfg.Sources[:idx], s.cfg.Sources[idx+1:]...)
	if err := s.persistConfig(); err != nil {
		s.log.Warn("source persist failed", zap.Error(err))
	}
	s.log.Info("source removed", zap.Int("idx", idx))
	jsonOK(w, map[string]string{"status": "ok"})
}

// --- Managed runtime handlers ---

// RuntimeInfo is the JSON shape returned by GET /api/runtimes.
type RuntimeInfo struct {
	Name           string `json:"name"`
	DisplayName    string `json:"display_name"`
	Description    string `json:"description"`
	DefaultVersion string `json:"default_version"`
	Version        string `json:"version"`   // configured version (empty = use default)
	Installed      bool   `json:"installed"` // binary present on disk?
}

// apiListRuntimes returns the status of every managed runtime.
func (s *Server) apiListRuntimes(w http.ResponseWriter, r *http.Request) {
	var out []RuntimeInfo
	for _, mgr := range s.managedRuntimes {
		version := ""
		if rc, ok := s.cfg.Runtimes[mgr.Name()]; ok {
			version = rc.Version
		}
		effectiveVersion := version
		if effectiveVersion == "" {
			effectiveVersion = mgr.DefaultVersion()
		}
		out = append(out, RuntimeInfo{
			Name:           mgr.Name(),
			DisplayName:    mgr.DisplayName(),
			Description:    mgr.Description(),
			DefaultVersion: mgr.DefaultVersion(),
			Version:        version,
			Installed:      mgr.IsInstalled(effectiveVersion),
		})
	}
	jsonOK(w, out)
}

// apiInstallRuntime downloads and caches the binary for a managed runtime,
// updates dicode.yaml, and registers the executor in the running engine.
func (s *Server) apiInstallRuntime(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var mgr pkgruntime.ManagedRuntime
	for _, m := range s.managedRuntimes {
		if m.Name() == name {
			mgr = m
			break
		}
	}
	if mgr == nil {
		jsonErr(w, "unknown runtime: "+name, http.StatusNotFound)
		return
	}

	if err := r.ParseForm(); err != nil {
		jsonErr(w, "bad request", http.StatusBadRequest)
		return
	}
	version := strings.TrimSpace(r.FormValue("version"))
	if version == "" {
		if rc, ok := s.cfg.Runtimes[name]; ok && rc.Version != "" {
			version = rc.Version
		} else {
			version = mgr.DefaultVersion()
		}
	}

	s.log.Info("installing runtime", zap.String("runtime", name), zap.String("version", version))
	if err := mgr.Install(r.Context(), version); err != nil {
		s.log.Error("runtime install failed", zap.String("runtime", name), zap.Error(err))
		jsonErr(w, "install failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Persist version to config.
	if s.cfg.Runtimes == nil {
		s.cfg.Runtimes = make(map[string]config.RuntimeConfig)
	}
	rc := s.cfg.Runtimes[name]
	rc.Version = version
	s.cfg.Runtimes[name] = rc
	if err := s.persistConfig(); err != nil {
		s.log.Warn("runtime config persist failed", zap.Error(err))
	}

	// Register the executor in the running engine.
	if path, err := mgr.BinaryPath(version); err == nil {
		s.engine.RegisterExecutor(task.Runtime(name), mgr.NewExecutor(path))
		s.log.Info("runtime registered in engine", zap.String("runtime", name), zap.String("version", version))
	}

	jsonOK(w, map[string]string{"status": "ok", "version": version})
}

// apiRemoveRuntime removes a runtime from dicode.yaml.
// The executor is NOT unregistered from the engine (in-flight runs must finish).
func (s *Server) apiRemoveRuntime(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "deno" {
		jsonErr(w, "deno is required and cannot be removed", http.StatusBadRequest)
		return
	}
	if s.cfg.Runtimes != nil {
		delete(s.cfg.Runtimes, name)
	}
	if err := s.persistConfig(); err != nil {
		s.log.Warn("runtime config persist failed", zap.Error(err))
	}
	jsonOK(w, map[string]string{"status": "ok"})
}

// persistConfig writes the current in-memory config back to dicode.yaml.
// It reads the existing file as a generic map (to preserve unknown keys),
// merges the changed sections, and writes back.
func (s *Server) persistConfig() error {
	if s.cfgPath == "" {
		return nil // no path configured (e.g. in tests)
	}

	// Read existing YAML as a generic document to preserve unknown keys.
	raw, err := os.ReadFile(s.cfgPath)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return err
	}
	if doc == nil {
		doc = make(map[string]any)
	}

	// Merge AI section.
	aiMap := map[string]any{
		"base_url":    s.cfg.AI.BaseURL,
		"model":       s.cfg.AI.Model,
		"api_key_env": s.cfg.AI.APIKeyEnv,
	}
	if s.cfg.AI.APIKey != "" {
		aiMap["api_key"] = s.cfg.AI.APIKey
	}
	doc["ai"] = aiMap

	// Merge top-level fields.
	doc["log_level"] = s.cfg.LogLevel

	// Merge server section carefully.
	serverMap, _ := doc["server"].(map[string]any)
	if serverMap == nil {
		serverMap = map[string]any{}
	}
	serverMap["port"] = s.cfg.Server.Port
	serverMap["secret"] = s.cfg.Server.Secret
	if s.cfg.Server.Tray != nil {
		serverMap["tray"] = *s.cfg.Server.Tray
	}
	doc["server"] = serverMap

	// Merge runtimes section.
	if len(s.cfg.Runtimes) > 0 {
		rtMap := make(map[string]any, len(s.cfg.Runtimes))
		for rtName, rc := range s.cfg.Runtimes {
			entry := map[string]any{}
			if rc.Version != "" {
				entry["version"] = rc.Version
			}
			if rc.Disabled {
				entry["disabled"] = true
			}
			rtMap[rtName] = entry
		}
		doc["runtimes"] = rtMap
	} else {
		delete(doc, "runtimes")
	}

	// Serialize sources list.
	if len(s.cfg.Sources) > 0 {
		var srcs []map[string]any
		for _, sc := range s.cfg.Sources {
			m := map[string]any{"type": string(sc.Type)}
			switch sc.Type {
			case config.SourceTypeLocal:
				m["path"] = sc.Path
			case config.SourceTypeGit:
				m["url"] = sc.URL
				if sc.Branch != "" {
					m["branch"] = sc.Branch
				}
				if sc.PollInterval > 0 {
					m["poll_interval"] = sc.PollInterval.String()
				}
				if sc.Auth.TokenEnv != "" {
					m["auth"] = map[string]any{"type": "token", "token_env": sc.Auth.TokenEnv}
				} else if sc.Auth.SSHKey != "" {
					m["auth"] = map[string]any{"type": "ssh", "ssh_key": sc.Auth.SSHKey}
				}
			}
			srcs = append(srcs, m)
		}
		doc["sources"] = srcs
	} else {
		doc["sources"] = []any{}
	}

	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(s.cfgPath, out, 0600)
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
	if tc.Daemon {
		restart := tc.Restart
		if restart == "" {
			restart = "always"
		}
		return "daemon (restart: " + restart + ")"
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
