// Package webui serves the REST API and SPA dashboard.
package webui

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"io/fs"
	"encoding/json"
	"fmt"
	"io"
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

//go:embed static
var staticFS embed.FS

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
	ws              *WSHub
	log             *zap.Logger
	port            int
	srv             *http.Server
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
	ss := newSessionStore()
	go ss.purgeLoop()

	wsHub := NewWSHub(log)

	s := &Server{
		registry:   r,
		engine:     eng,
		cfg:        cfg,
		cfgPath:    cfgPath,
		secretsMgr: secretsMgr,
		reconciler: rec,
		dataDir:    dataDir,
		sessions:   ss,
		limiter:    newUnlockLimiter(),
		logs:       logs,
		ws:         wsHub,
		log:        log,
		port:       port,
	}

	// Wire run started hook → broadcast run:started
	eng.SetRunStartedHook(func(taskID, runID, triggerSource string) {
		taskName := taskID
		if spec, ok := r.Get(taskID); ok {
			taskName = spec.Name
		}
		s.ws.Broadcast(WSMsg{
			Type: "run:started",
			Data: RunStartedData{
				RunID:         runID,
				TaskID:        taskID,
				TaskName:      taskName,
				TriggerSource: triggerSource,
			},
		})
	})

	// Wire run finished hook → broadcast run:finished
	eng.SetRunFinishedHook(func(taskID, runID, status, triggerSource string, durationMs int64) {
		taskName := taskID
		var outputContentType, returnValue string
		if spec, ok := r.Get(taskID); ok {
			taskName = spec.Name
		}
		if run, err := r.GetRun(context.Background(), runID); err == nil {
			outputContentType = run.OutputContentType
			returnValue = run.ReturnValue
		}
		s.ws.Broadcast(WSMsg{
			Type: "run:finished",
			Data: RunFinishedData{
				RunID:             runID,
				TaskID:            taskID,
				TaskName:          taskName,
				Status:            status,
				DurationMs:        durationMs,
				TriggerSource:     triggerSource,
				OutputContentType: outputContentType,
				ReturnValue:       returnValue,
			},
		})
	})

	// Wire registry log hook → broadcast run:log
	r.SetLogHook(func(runID, level, msg string, ts int64) {
		s.ws.Broadcast(WSMsg{
			Type: "run:log",
			Data: RunLogData{
				RunID:   runID,
				Level:   level,
				Message: msg,
				Ts:      ts,
			},
		})
	})

	// Wire log broadcaster hook → ws BroadcastLog + replay buffer
	if logs != nil {
		logs.SetHook(s.ws.BroadcastLog)
		s.ws.recentLogs = logs.Recent
	}

	return s, nil
}

// Handler returns the HTTP handler (useful for testing without starting a server).
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.Logger)
	r.Use(securityHeaders)

	// WebSocket
	r.Get("/ws", s.ws.ServeHTTP)

	// Service worker
	r.Get("/sw.js", s.handleServiceWorker)

	// Run result (bare page, no chrome)
	r.Get("/runs/{runID}/result", s.handleRunResult)

	// File editor API (task.js / task.test.js only)
	r.Get("/api/tasks/{id}/files/{filename}", s.apiGetFile)
	r.Post("/api/tasks/{id}/files/{filename}", s.apiSaveFile)
	r.Post("/api/tasks/{id}/trigger", s.apiSaveTrigger)

	// AI chat — streams SSE, writes task files live
	r.Post("/api/tasks/{id}/ai/stream", s.handleAIStream)

	// REST API with CORS
	r.Route("/api", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
				if r.Method == http.MethodOptions {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				next.ServeHTTP(w, r)
			})
		})

		r.Get("/config", s.apiGetConfig)
		r.Get("/config/raw", s.apiGetConfigRaw)
		r.Post("/config/raw", s.apiSaveConfigRaw)

		r.Get("/tasks", s.apiListTasks)
		r.Get("/tasks/{id}", s.apiGetTask)
		r.Post("/tasks/{id}/run", s.apiRunTask)
		r.Get("/tasks/{id}/runs", s.apiListRuns)
		r.Get("/tasks/{id}/files/{filename}", s.apiGetFile)
		r.Post("/tasks/{id}/files/{filename}", s.apiSaveFile)
		r.Post("/tasks/{id}/trigger", s.apiSaveTrigger)
		r.Post("/tasks/{id}/ai/stream", s.handleAIStream)

		r.Get("/runs/{runID}", s.apiGetRun)
		r.Get("/runs/{runID}/logs", s.apiGetLogs)
		r.Post("/runs/{runID}/kill", s.apiKillRun)

		// Secrets management
		r.Get("/secrets", s.apiListSecrets)
		r.Post("/secrets", s.apiSetSecret)
		r.Delete("/secrets/{key}", s.apiDeleteSecret)
		r.Post("/secrets/unlock", s.apiSecretsUnlock)
		r.Post("/secrets/lock", s.apiSecretsLock)

		// Settings
		r.Post("/settings/ai", s.apiSaveAISettings)
		r.Post("/settings/server", s.apiSaveServerSettings)
		r.Post("/settings/sources", s.apiAddSource)
		r.Delete("/settings/sources/{idx}", s.apiRemoveSource)
		r.Get("/settings/sources/git/branches", s.apiListGitBranches)

		// Managed runtime lifecycle
		r.Get("/runtimes", s.apiListRuntimes)
		r.Post("/runtimes/{name}/install", s.apiInstallRuntime)
		r.Delete("/runtimes/{name}", s.apiRemoveRuntime)
	})

	// Webhook passthrough
	r.Post("/hooks/*", func(w http.ResponseWriter, req *http.Request) {
		req.URL.Path = "/hooks/" + chi.URLParam(req, "*")
		s.engine.WebhookHandler().ServeHTTP(w, req)
	})

	// Static SPA assets (/app/app.js, etc.)
	appFS, _ := fs.Sub(staticFS, "static")
	r.Handle("/app/*", http.FileServer(http.FS(appFS)))

	// SPA catch-all — serve index.html for all unmatched GET routes
	r.Get("/*", s.serveSPA)

	return r
}

// Start listens on the configured port until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	s.srv = &http.Server{
		Addr:              fmt.Sprintf(":%d", s.port),
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
		// WriteTimeout is intentionally 0: WebSocket, SSE and AI stream endpoints write indefinitely.
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

// serveSPA serves the SPA index.html for all unmatched GET routes.
func (s *Server) serveSPA(w http.ResponseWriter, r *http.Request) {
	b, err := staticFS.ReadFile("static/app/index.html")
	if err != nil {
		http.Error(w, "SPA not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(b) //nolint:errcheck
}

// handleRunResult serves only the structured output of a run (bare page, no chrome).
func (s *Server) handleRunResult(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	run, err := s.registry.GetRun(r.Context(), runID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if run.OutputContentType == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", run.OutputContentType+"; charset=utf-8")
	_, _ = w.Write([]byte(run.OutputContent))
}

// handleServiceWorker serves the Service Worker JS file.
func (s *Server) handleServiceWorker(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("Service-Worker-Allowed", "/")
	b, err := staticFS.ReadFile("static/sw.js")
	if err != nil {
		http.Error(w, "sw.js not found", http.StatusNotFound)
		return
	}
	w.Write(b) //nolint:errcheck
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

// apiSaveConfigRaw validates and writes the raw config back to dicode.yaml.
func (s *Server) apiSaveConfigRaw(w http.ResponseWriter, r *http.Request) {
	if !s.secretsSessionValid(r) {
		jsonErr(w, "unlock secrets first to edit raw config", http.StatusUnauthorized)
		return
	}
	if s.cfgPath == "" {
		jsonErr(w, "config file path not set", http.StatusBadRequest)
		return
	}

	// Support both JSON body and form value.
	var content string
	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "application/json") {
		var body struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		content = body.Content
	} else {
		content = r.FormValue("content")
	}

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

// allowedFiles restricts which files the editor API can read/write.
var allowedFiles = map[string]bool{
	"task.js": true, "task.ts": true, "task.py": true,
	"task.test.js": true, "task.test.ts": true,
	"Dockerfile": true,
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
	// Return plain text so the SPA can use it directly
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(b) //nolint:errcheck
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

	// Accept either plain text body or form value "content"
	var content string
	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "text/plain") {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			jsonErr(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		content = string(b)
	} else {
		content = r.FormValue("content")
	}

	if err := os.WriteFile(filepath.Join(spec.TaskDir, filename), []byte(content), 0600); err != nil {
		jsonErr(w, "save failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("file saved", zap.String("task", id), zap.String("file", filename))
	jsonOK(w, map[string]string{"status": "saved"})
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

	// Parse trigger from JSON body.
	var body struct {
		Type    string `json:"type"`
		Cron    string `json:"cron"`
		Webhook string `json:"webhook"`
		From    string `json:"from"`
		On      string `json:"on"`
		Restart string `json:"restart"`
	}
	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
	} else {
		// Fallback: form values
		body.Type = r.FormValue("type")
		body.Cron = r.FormValue("cron")
		body.Webhook = r.FormValue("webhook")
		body.From = r.FormValue("chain_from")
		body.On = r.FormValue("chain_on")
		body.Restart = r.FormValue("restart")
	}

	var trigMap map[string]any
	switch body.Type {
	case "cron":
		trigMap = map[string]any{"cron": body.Cron}
	case "webhook":
		trigMap = map[string]any{"webhook": body.Webhook}
	case "manual":
		trigMap = map[string]any{"manual": true}
	case "chain":
		chain := map[string]any{"from": body.From}
		if body.On != "" && body.On != "success" {
			chain["on"] = body.On
		}
		trigMap = map[string]any{"chain": chain}
	case "daemon":
		trigMap = map[string]any{"daemon": true}
		if body.Restart != "" && body.Restart != "always" {
			trigMap["restart"] = body.Restart
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
	s.log.Info("trigger saved", zap.String("task", id), zap.String("type", body.Type))
	jsonOK(w, map[string]string{"status": "saved"})
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

func (s *Server) requireSecretsSession(w http.ResponseWriter, r *http.Request) bool {
	if s.secretsMgr == nil {
		jsonErr(w, "secrets not configured", http.StatusServiceUnavailable)
		return false
	}
	if !s.secretsSessionValid(r) {
		jsonErr(w, "secrets locked — unlock via POST /api/secrets/unlock", http.StatusUnauthorized)
		return false
	}
	return true
}

// apiSecretsUnlock accepts {"password":"..."} and issues a session cookie.
func (s *Server) apiSecretsUnlock(w http.ResponseWriter, r *http.Request) {
	// Rate-limit by client IP.
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if !s.limiter.allow(ip) {
		jsonErr(w, "too many unlock attempts — try again in a minute", http.StatusTooManyRequests)
		return
	}

	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	expected := s.secretsPassphrase()
	if expected != "" && subtle.ConstantTimeCompare([]byte(body.Password), []byte(expected)) != 1 {
		jsonErr(w, "incorrect password", http.StatusUnauthorized)
		return
	}

	token := s.sessions.issue(expected + "dicode")
	http.SetCookie(w, &http.Cookie{
		Name:     secretsCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   8 * 3600,
	})
	jsonOK(w, map[string]string{"status": "ok"})
}

// apiSecretsLock revokes the current session cookie.
func (s *Server) apiSecretsLock(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(secretsCookie); err == nil {
		s.sessions.revoke(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: secretsCookie, Path: "/", MaxAge: -1})
	jsonOK(w, map[string]string{"status": "ok"})
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

	var key, value string
	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "application/json") {
		var body struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		key = body.Key
		value = body.Value
	} else {
		key = r.FormValue("key")
		value = r.FormValue("value")
	}

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

// --- REST API handlers ---

func (s *Server) apiGetConfig(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, s.cfg)
}

// TaskListItem is the shape returned by GET /api/tasks.
type TaskListItem struct {
	*task.Spec
	TriggerLabel  string `json:"trigger_label"`
	LastRunID     string `json:"last_run_id,omitempty"`
	LastRunStatus string `json:"last_run_status,omitempty"`
}

func (s *Server) apiListTasks(w http.ResponseWriter, r *http.Request) {
	specs := s.registry.All()
	items := make([]TaskListItem, len(specs))
	for i, spec := range specs {
		item := TaskListItem{
			Spec:         spec,
			TriggerLabel: triggerLabel(spec.Trigger),
		}
		if runs, err := s.registry.ListRuns(r.Context(), spec.ID, 1); err == nil && len(runs) > 0 {
			item.LastRunID = runs[0].ID
			item.LastRunStatus = runs[0].Status
		}
		items[i] = item
	}
	jsonOK(w, items)
}

// TaskDetail is the shape returned by GET /api/tasks/{id}.
type TaskDetail struct {
	*task.Spec
	TriggerLabel string `json:"trigger_label"`
	ScriptFile   string `json:"script_file"`
	TestFile     string `json:"test_file"`
	TestExists   bool   `json:"test_exists"`
}

func (s *Server) apiGetTask(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	spec, ok := s.registry.Get(id)
	if !ok {
		jsonErr(w, "task not found", http.StatusNotFound)
		return
	}

	detail := TaskDetail{
		Spec:         spec,
		TriggerLabel: triggerLabel(spec.Trigger),
	}

	// Determine script file
	switch spec.Runtime {
	case task.RuntimeDocker, task.RuntimePodman:
		detail.ScriptFile = "Dockerfile"
	default:
		for _, name := range []string{"task.ts", "task.js", "task.py"} {
			if _, err := os.Stat(filepath.Join(spec.TaskDir, name)); err == nil {
				detail.ScriptFile = name
				break
			}
		}
		if detail.ScriptFile == "" {
			detail.ScriptFile = "task.ts"
		}
		for _, name := range []string{"task.test.ts", "task.test.js"} {
			if _, err := os.Stat(filepath.Join(spec.TaskDir, name)); err == nil {
				detail.TestFile = name
				detail.TestExists = true
				break
			}
		}
		if detail.TestFile == "" {
			if strings.HasSuffix(detail.ScriptFile, ".ts") {
				detail.TestFile = "task.test.ts"
			} else {
				detail.TestFile = "task.test.js"
			}
		}
	}

	jsonOK(w, detail)
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
	limitStr := r.URL.Query().Get("limit")
	limit := 50
	if limitStr != "" {
		fmt.Sscanf(limitStr, "%d", &limit)
	}
	runs, err := s.registry.ListRuns(r.Context(), id, limit)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, runs)
}

// RunDetail is the shape returned by GET /api/runs/{runID}.
type RunDetail struct {
	*registry.Run
	TaskName string `json:"task_name"`
}

func (s *Server) apiGetRun(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	run, err := s.registry.GetRun(r.Context(), runID)
	if err != nil {
		jsonErr(w, "run not found", http.StatusNotFound)
		return
	}
	taskName := run.TaskID
	if spec, ok := s.registry.Get(run.TaskID); ok {
		taskName = spec.Name
	}
	jsonOK(w, RunDetail{Run: run, TaskName: taskName})
}

// LogEntryJSON is the JSON shape returned by GET /api/runs/{runID}/logs.
type LogEntryJSON struct {
	ID      int64  `json:"id"`
	Level   string `json:"level"`
	Message string `json:"message"`
	Ts      int64  `json:"ts"` // Unix milliseconds
}

func (s *Server) apiGetLogs(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")

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

	out := make([]LogEntryJSON, len(logs))
	for i, l := range logs {
		out[i] = LogEntryJSON{
			ID:      l.ID,
			Level:   l.Level,
			Message: l.Message,
			Ts:      l.Ts.UnixMilli(),
		}
	}
	jsonOK(w, out)
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

func (s *Server) apiSaveAISettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BaseURL     string `json:"base_url"`
		Model       string `json:"model"`
		APIKeyEnv   string `json:"api_key_env"`
		APIKey      string `json:"api_key"`
		ClearAPIKey bool   `json:"clear_api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, "bad request", http.StatusBadRequest)
		return
	}
	s.cfg.AI.BaseURL = strings.TrimSpace(body.BaseURL)
	s.cfg.AI.Model = strings.TrimSpace(body.Model)
	s.cfg.AI.APIKeyEnv = strings.TrimSpace(body.APIKeyEnv)
	if body.APIKey != "" {
		s.cfg.AI.APIKey = body.APIKey
	} else if body.ClearAPIKey {
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

func (s *Server) apiSaveServerSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LogLevel string `json:"log_level"`
		Secret   string `json:"secret"`
		Tray     *bool  `json:"tray"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, "bad request", http.StatusBadRequest)
		return
	}
	if body.LogLevel != "" {
		s.cfg.LogLevel = body.LogLevel
	}
	if body.Secret != "" {
		s.cfg.Server.Secret = body.Secret
	}
	if body.Tray != nil {
		s.cfg.Server.Tray = body.Tray
	}
	if err := s.persistConfig(); err != nil {
		s.log.Warn("settings persist failed", zap.Error(err))
		jsonErr(w, "saved in memory but could not write file: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("server settings updated")
	jsonOK(w, map[string]string{"status": "ok"})
}

func (s *Server) apiAddSource(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Type     string `json:"type"`
		Path     string `json:"path"`
		URL      string `json:"url"`
		Branch   string `json:"branch"`
		TokenEnv string `json:"token_env"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, "bad request", http.StatusBadRequest)
		return
	}
	var sc config.SourceConfig
	switch config.SourceType(body.Type) {
	case config.SourceTypeLocal:
		path := strings.TrimSpace(body.Path)
		if path == "" {
			jsonErr(w, "path is required for local source", http.StatusBadRequest)
			return
		}
		sc = config.SourceConfig{Type: config.SourceTypeLocal, Path: path, Watch: true}
	case config.SourceTypeGit:
		url := strings.TrimSpace(body.URL)
		if url == "" {
			jsonErr(w, "url is required for git source", http.StatusBadRequest)
			return
		}
		branch := body.Branch
		if branch == "" {
			branch = "main"
		}
		sc = config.SourceConfig{
			Type:         config.SourceTypeGit,
			URL:          url,
			Branch:       branch,
			PollInterval: 30 * 1e9,
			Auth:         config.SourceAuth{TokenEnv: body.TokenEnv},
		}
	default:
		jsonErr(w, "type must be 'local' or 'git'", http.StatusBadRequest)
		return
	}

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

	s.cfg.Sources = append(s.cfg.Sources, sc)
	if err := s.persistConfig(); err != nil {
		s.log.Warn("source persist failed", zap.Error(err))
	}
	s.log.Info("source added", zap.String("type", body.Type))
	jsonOK(w, map[string]string{"status": "ok"})
}

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

type RuntimeInfo struct {
	Name           string `json:"name"`
	DisplayName    string `json:"display_name"`
	Description    string `json:"description"`
	DefaultVersion string `json:"default_version"`
	Version        string `json:"version"`
	Installed      bool   `json:"installed"`
}

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

	if s.cfg.Runtimes == nil {
		s.cfg.Runtimes = make(map[string]config.RuntimeConfig)
	}
	rc := s.cfg.Runtimes[name]
	rc.Version = version
	s.cfg.Runtimes[name] = rc
	if err := s.persistConfig(); err != nil {
		s.log.Warn("runtime config persist failed", zap.Error(err))
	}

	if path, err := mgr.BinaryPath(version); err == nil {
		s.engine.RegisterExecutor(task.Runtime(name), mgr.NewExecutor(path))
		s.log.Info("runtime registered in engine", zap.String("runtime", name), zap.String("version", version))
	}

	jsonOK(w, map[string]string{"status": "ok", "version": version})
}

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
func (s *Server) persistConfig() error {
	if s.cfgPath == "" {
		return nil
	}

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

	aiMap := map[string]any{
		"base_url":    s.cfg.AI.BaseURL,
		"model":       s.cfg.AI.Model,
		"api_key_env": s.cfg.AI.APIKeyEnv,
	}
	if s.cfg.AI.APIKey != "" {
		aiMap["api_key"] = s.cfg.AI.APIKey
	}
	doc["ai"] = aiMap

	doc["log_level"] = s.cfg.LogLevel

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
			if len(entry) == 0 {
				continue
			}
			rtMap[rtName] = entry
		}
		doc["runtimes"] = rtMap
	} else {
		delete(doc, "runtimes")
	}

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
// Kept for internal use.
type TaskRow struct {
	*task.Spec
	LastStatus string
	LastRunID  string
	LastRunAt  *time.Time
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
