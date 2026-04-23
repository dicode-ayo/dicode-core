// Package webui serves the REST API and SPA dashboard.
package webui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/mcp"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/relay"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	denoruntime "github.com/dicode/dicode/pkg/runtime/deno"
	"github.com/dicode/dicode/pkg/secrets"
	gitSource "github.com/dicode/dicode/pkg/source/git"
	"github.com/dicode/dicode/pkg/source/local"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/trigger"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// SecretsManager is an alias for secrets.Manager kept for call-site clarity.
type SecretsManager = secrets.Manager

// sessionStore holds in-memory session tokens for the secrets page.
type sessionStore struct {
	mu     sync.Mutex
	tokens map[string]time.Time
}

func newSessionStore() *sessionStore { return &sessionStore{tokens: make(map[string]time.Time)} }

func (s *sessionStore) issue() string {
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	token := hex.EncodeToString(raw)
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
	unlockLockoutTTL  = 15 * time.Minute // extended lockout after max attempts
)

// webhookPathPrefix is the URL prefix every webhook-triggered task's HTTP
// surface lives under. Anything outside it is infrastructure or SPA routing
// — the /api/ai/chat forward guard, webhookAuthGuard's public-path carve-out,
// the /hooks/* mux entry, and the slug-to-task resolver all share this.
// Keep the trailing slash to enforce boundary matching (TrimPrefix + HasPrefix
// semantics require it).
const webhookPathPrefix = "/hooks/"

func newUnlockLimiter() *unlockLimiter {
	return &unlockLimiter{entries: make(map[string]*limitEntry)}
}

func (l *unlockLimiter) allow(ip string) bool {
	// Test/dev escape hatch: disable the limiter entirely when this env is set.
	// Prod never sets it; e2e tests rapid-fire many login attempts from one IP
	// and would otherwise trip the 5-per-minute cap mid-suite.
	if os.Getenv("DICODE_DISABLE_UNLOCK_LIMITER") == "1" {
		return true
	}
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
	if e.count >= unlockMaxAttempts {
		// Extend the lockout window significantly on the attempt that hits the cap.
		e.resetAt = now.Add(unlockLockoutTTL)
	}
	return true
}

//go:embed static
var staticFS embed.FS

// Server is the HTTP server for the web UI and REST API.
type Server struct {
	registry           *registry.Registry
	engine             *trigger.Engine
	cfg                *config.Config
	cfgPath            string               // path to dicode.yaml; empty in tests
	secretsMgr         SecretsManager       // nil if local provider not configured
	reconciler         *registry.Reconciler // nil if not wired
	sourceMgr          *SourceManager       // nil if not wired
	dataDir            string               // ~/.dicode or cfg.DataDir
	gateway            *ipc.Gateway
	relayClient        *relay.Client
	managedRuntimes    []pkgruntime.ManagedRuntime
	sessions           *sessionStore
	dbSessions         *dbSessionStore  // persistent sessions / trusted devices
	apiKeys            *apiKeyStore     // MCP / programmatic API keys
	passphraseStore    *passphraseStore // auth passphrase persisted in DB
	cachedPassphrase   string           // in-memory cache of resolved passphrase; invalidated on change
	cachedPassphraseMu sync.RWMutex
	limiter            *unlockLimiter
	logs               *LogBroadcaster
	ws                 *WSHub
	log                *zap.Logger
	port               int
	srv                *http.Server
}

// SetRelayClient stores a reference to the relay client so the API can expose
// the relay hook base URL to the web UI.
func (s *Server) SetRelayClient(rc *relay.Client) {
	s.relayClient = rc
}

// SetManagedRuntimes registers the list of managed runtimes (Deno, Python, …)
// that will appear in the Config UI. Call this after New and before Start.
func (s *Server) SetManagedRuntimes(runtimes []pkgruntime.ManagedRuntime) {
	s.managedRuntimes = runtimes
}

// New creates a Server. cfgPath is the path to dicode.yaml used to persist
// settings changes; pass "" in tests or when persistence is not needed.
// rec and dataDir enable live source management; pass nil/"" in tests.
// sourceMgr enables the /api/sources endpoints and MCP source tools; pass nil in tests.
// database is required for persistent sessions and API key storage; pass nil in tests (auth features disabled).
func New(port int, r *registry.Registry, eng *trigger.Engine, cfg *config.Config, cfgPath string, secretsMgr SecretsManager, rec *registry.Reconciler, sourceMgr *SourceManager, dataDir string, logs *LogBroadcaster, log *zap.Logger, database db.DB, gateway *ipc.Gateway) (*Server, error) {
	ss := newSessionStore()
	go ss.purgeLoop()

	wsHub := NewWSHub(log)

	var dbs *dbSessionStore
	var aks *apiKeyStore
	var ps *passphraseStore
	if database != nil {
		dbs = newDBSessionStore(database)
		aks = newAPIKeyStore(database)
		ps = newPassphraseStore(database)
	}

	s := &Server{
		registry:        r,
		engine:          eng,
		cfg:             cfg,
		cfgPath:         cfgPath,
		secretsMgr:      secretsMgr,
		reconciler:      rec,
		sourceMgr:       sourceMgr,
		dataDir:         dataDir,
		sessions:        ss,
		dbSessions:      dbs,
		apiKeys:         aks,
		passphraseStore: ps,
		limiter:         newUnlockLimiter(),
		logs:            logs,
		ws:              wsHub,
		log:             log,
		port:            port,
		gateway:         gateway,
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
	eng.SetRunFinishedHook(func(taskID, runID, status, triggerSource string, durationMs int64, notifyOnSuccess, notifyOnFailure bool) {
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
				NotifyOnSuccess:   notifyOnSuccess,
				NotifyOnFailure:   notifyOnFailure,
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

	// Wire reconciler hooks → broadcast tasks:changed when tasks are added/removed.
	// Chain with the existing callbacks (already wired to the trigger engine in main).
	if rec != nil {
		prev := rec.OnRegister
		rec.OnRegister = func(spec *task.Spec) {
			if prev != nil {
				prev(spec)
			}
			s.ws.Broadcast(WSMsg{Type: "tasks:changed"})
		}
		prevUn := rec.OnUnregister
		rec.OnUnregister = func(id string) {
			if prevUn != nil {
				prevUn(id)
			}
			s.ws.Broadcast(WSMsg{Type: "tasks:changed"})
		}
	}

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
	r.Use(useEncodedPath)
	// RequestLogger must wrap Recoverer: chi's Recoverer reads the LogEntry
	// from the request context that RequestLogger installs, and routes panics
	// to its Panic method. Reversing the order silently drops panic logging.
	r.Use(middleware.RequestID)
	r.Use(middleware.RequestLogger(&zapLogFormatter{log: s.log}))
	r.Use(middleware.Recoverer)
	r.Use(securityHeaders)

	// Auth endpoints — always public (login flow must be reachable without session).
	r.Post("/api/auth/login", s.apiSecretsUnlock)
	r.Post("/api/auth/refresh", s.apiAuthRefresh)
	r.Get("/login", s.handleLoginPage)

	// Webhook passthrough — auth via per-task HMAC secret or optional session cookie.
	// When a task sets trigger.auth: true, a valid dicode session is required for
	// both GET (serving the task UI) and POST (running the task). Public webhooks
	// (no auth: true) remain fully open.
	webhookHandler := func(w http.ResponseWriter, req *http.Request) {
		req.URL.Path = webhookPathPrefix + chi.URLParam(req, "*")
		s.webhookAuthGuard(w, req, s.gateway)
	}
	r.Get("/hooks/*", webhookHandler)
	r.Post("/hooks/*", webhookHandler)

	// sdk.d.ts — TypeScript declarations for Monaco IntelliSense (public, no auth required).
	r.Get("/api/sdk/types", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/typescript; charset=utf-8")
		_, _ = w.Write(denoruntime.SdkDts)
	})

	// dicode.js — client SDK injected into webhook task UIs (public, no auth required).
	r.Get("/dicode.js", func(w http.ResponseWriter, req *http.Request) {
		b, err := staticFS.ReadFile("static/dicode.js")
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		_, _ = w.Write(b)
	})

	// dicode-oauth-broadcast.js — loaded from OAuth success pages to signal
	// peer tabs that a secret has been stored. Public: the script carries
	// no capabilities beyond posting a BroadcastChannel message whose
	// contents are read from its own query string. See source for details.
	r.Get("/dicode-oauth-broadcast.js", func(w http.ResponseWriter, req *http.Request) {
		b, err := staticFS.ReadFile("static/dicode-oauth-broadcast.js")
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		_, _ = w.Write(b)
	})

	// Everything below this point requires a valid session when auth is enabled.
	r.Group(func(r chi.Router) {
		r.Use(s.requireAuth)
		r.Use(s.corsMiddleware)

		// WebSocket
		r.Get("/ws", s.ws.ServeHTTP)

		// Run result (bare page, no chrome)
		r.Get("/runs/{runID}/result", s.handleRunResult)

		// File editor API (task.js / task.test.js only)
		r.Get("/api/tasks/{id}/files/{filename}", s.apiGetFile)
		r.Post("/api/tasks/{id}/files/{filename}", s.apiSaveFile)
		r.Post("/api/tasks/{id}/trigger", s.apiSaveTrigger)

		r.Route("/api", func(r chi.Router) {
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

			r.Get("/runs/{runID}", s.apiGetRun)
			r.Get("/runs/{runID}/logs", s.apiGetLogs)
			r.Post("/runs/{runID}/kill", s.apiKillRun)

			// Secrets management (protected by main session via requireAuth above).
			// GET returns key names only — values are never surfaced via API.
			r.Get("/secrets", s.apiListSecrets)
			r.Post("/secrets", s.apiSetSecret)
			r.Delete("/secrets/{key}", s.apiDeleteSecret)

			// Auth management — trusted devices, API keys & passphrase
			r.Get("/auth/devices", s.apiListDevices)
			r.Delete("/auth/devices/{id}", s.apiRevokeDevice)
			r.Post("/auth/logout", s.apiLogout)
			r.Post("/auth/logout-all", s.apiLogoutAll)
			r.Get("/auth/keys", s.apiListAPIKeys)
			r.Post("/auth/keys", s.apiCreateAPIKey)
			r.Delete("/auth/keys/{id}", s.apiRevokeAPIKey)
			r.Get("/auth/passphrase", s.apiGetPassphraseStatus)
			r.Post("/auth/passphrase", s.apiChangePassphrase)

			// Settings
			r.Post("/settings/server", s.apiSaveServerSettings)
			r.Post("/settings/ai", s.apiSaveAISettings)
			r.Post("/settings/sources", s.apiAddSource)
			r.Delete("/settings/sources/{idx}", s.apiRemoveSource)
			r.Get("/settings/sources/git/branches", s.apiListGitBranches)

			// Relay status (#87) — returns {"enabled":false} when disabled.
			r.Get("/relay/status", s.apiRelayStatus)

			// Source management (taskset model)
			r.Get("/sources", s.apiListSources)
			r.Patch("/sources/{name}/dev", s.apiSetDevMode)
			r.Get("/sources/{name}/branches", s.apiListSourceBranches)

			// Metrics
			r.Get("/metrics", s.apiMetrics)

			// AI chat — forwards to the task named by cfg.AI.Task.
			r.Post("/ai/chat", s.apiAIChat)

			// Managed runtime lifecycle
			r.Get("/runtimes", s.apiListRuntimes)
			r.Post("/runtimes/{name}/install", s.apiInstallRuntime)
			r.Delete("/runtimes/{name}", s.apiRemoveRuntime)
		})

		// MCP endpoint — requires API key when auth is enabled.
		if s.cfg == nil || s.cfg.Server.MCP {
			mcpSrv := mcp.New(s.registry, s.sourceMgr)
			r.With(s.requireAPIKey).Mount("/mcp", mcpSrv.Handler())
		}

		// Redirect root and unmatched GET routes to the webui webhook task.
		r.Get("/*", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/hooks/webui", http.StatusFound)
		})
	})

	return r
}

// Start listens on the configured port until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	// Ensure an auth passphrase exists before accepting any connections.
	// Auto-generates and prints one if server.auth is true and none is configured.
	if err := s.ensurePassphrase(ctx); err != nil {
		return fmt.Errorf("ensure auth passphrase: %w", err)
	}

	s.srv = &http.Server{
		Addr:              fmt.Sprintf(":%d", s.port),
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
		// WriteTimeout is intentionally 0: WebSocket and SSE endpoints write indefinitely.
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutCtx)
	}()
	certFile := s.cfg.Server.TLSCertFile
	keyFile := s.cfg.Server.TLSKeyFile
	if certFile != "" && keyFile != "" {
		s.log.Info("webui listening (HTTPS)", zap.Int("port", s.port),
			zap.String("hint", fmt.Sprintf("set DICODE_BASE_URL secret to https://YOUR_HOST:%d", s.port)))
		if err := s.srv.ListenAndServeTLS(certFile, keyFile); err != nil && err != http.ErrServerClosed {
			return err
		}
	} else {
		s.log.Info("webui listening", zap.Int("port", s.port))
		if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			return err
		}
	}
	return nil
}

// useEncodedPath is a middleware that makes chi route on r.URL.RawPath instead
// of r.URL.Path, so percent-encoded slashes (%2F) in task IDs are treated as a
// single path segment rather than path separators.
func useEncodedPath(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if raw := r.URL.RawPath; raw != "" && raw != r.URL.Path {
			r2 := r.Clone(r.Context())
			r2.URL.Path = raw
			next.ServeHTTP(w, r2)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// taskIDParam returns the decoded task ID from the chi URL parameter "id".
// When task IDs contain slashes they are transmitted as %2F (via encodeURIComponent
// in the frontend), so we must URL-decode after chi captures the raw segment.
func taskIDParam(r *http.Request) string {
	id, err := url.PathUnescape(chi.URLParam(r, "id"))
	if err != nil {
		return chi.URLParam(r, "id")
	}
	return id
}

// handleRunResult serves only the structured output of a run (bare page, no chrome).
func (s *Server) handleRunResult(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	run, err := s.registry.GetRun(r.Context(), runID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if run.OutputContentType != "" {
		w.Header().Set("Content-Type", run.OutputContentType+"; charset=utf-8")
		_, _ = w.Write([]byte(run.OutputContent))
		return
	}
	if run.ReturnValue != "" {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(run.ReturnValue))
		return
	}
	http.NotFound(w, r)
}

// apiGetConfigRaw returns the raw content of dicode.yaml.
// Protected by the main session via requireAuth.
func (s *Server) apiGetConfigRaw(w http.ResponseWriter, r *http.Request) {
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
// Protected by the main session via requireAuth.
func (s *Server) apiSaveConfigRaw(w http.ResponseWriter, r *http.Request) {
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
	// Webhook UI files — editable via the built-in code editor.
	"index.html": true, "style.css": true, "script.js": true,
}

// safeTaskFilePath resolves filename inside taskDir with belt-and-suspenders
// path validation. Callers already gate on allowedFiles (an exact-match
// allowlist), but this function adds a second layer that static analysers
// recognise as a path-injection sanitiser:
//
//  1. Reject filenames containing any path separator or parent reference.
//  2. After Clean+Join, assert the absolute result is still rooted in the
//     absolute form of taskDir (filepath.Rel returns a path with no leading
//     "..").
//
// Returns an error when the candidate escapes taskDir.
func safeTaskFilePath(taskDir, filename string) (string, error) {
	if filename == "" ||
		strings.ContainsAny(filename, `/\`) ||
		filename == "." || filename == ".." ||
		filepath.Base(filename) != filename ||
		filepath.Clean(filename) != filename {
		return "", fmt.Errorf("invalid filename")
	}
	absDir, err := filepath.Abs(taskDir)
	if err != nil {
		return "", fmt.Errorf("task dir abs: %w", err)
	}
	joined := filepath.Join(absDir, filename)
	rel, err := filepath.Rel(absDir, joined)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || strings.ContainsRune(rel, filepath.Separator) {
		return "", fmt.Errorf("path escapes task dir")
	}
	return joined, nil
}

func (s *Server) apiGetFile(w http.ResponseWriter, r *http.Request) {
	id, filename := taskIDParam(r), chi.URLParam(r, "filename")
	if !allowedFiles[filename] {
		jsonErr(w, "file not allowed", http.StatusBadRequest)
		return
	}
	spec, ok := s.registry.Get(id)
	if !ok {
		jsonErr(w, "task not found", http.StatusNotFound)
		return
	}
	path, err := safeTaskFilePath(spec.TaskDir, filename)
	if err != nil {
		jsonErr(w, "invalid filename", http.StatusBadRequest)
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		jsonErr(w, "file not found", http.StatusNotFound)
		return
	}
	// Return plain text so the SPA can use it directly
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(b) //nolint:errcheck
}

func (s *Server) apiSaveFile(w http.ResponseWriter, r *http.Request) {
	id, filename := taskIDParam(r), chi.URLParam(r, "filename")
	if !allowedFiles[filename] {
		jsonErr(w, "file not allowed", http.StatusBadRequest)
		return
	}
	spec, ok := s.registry.Get(id)
	if !ok {
		jsonErr(w, "task not found", http.StatusNotFound)
		return
	}
	path, err := safeTaskFilePath(spec.TaskDir, filename)
	if err != nil {
		jsonErr(w, "invalid filename", http.StatusBadRequest)
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

	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		jsonErr(w, "save failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("file saved", zap.String("task", id), zap.String("file", filename))
	jsonOK(w, map[string]string{"status": "saved"})
}

func (s *Server) apiSaveTrigger(w http.ResponseWriter, r *http.Request) {
	id := taskIDParam(r)
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

const sessionCookie = "dicode_secrets_sess"

// apiSecretsUnlock accepts {"password":"...","trust":true,"next":"/path"} and
// issues a session cookie. When trust=true a long-lived device cookie is also
// issued so the browser is remembered across restarts (trusted-browser feature).
// HTML form posts (Content-Type application/x-www-form-urlencoded) receive a
// 303 redirect to the validated next path (or /hooks/webui). JSON posts always
// receive a JSON response; when next is present and safe it is echoed back so
// the SPA can navigate to it.
func (s *Server) apiSecretsUnlock(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r, s.cfg.Server.TrustProxy)
	if !s.limiter.allow(ip) {
		jsonErr(w, "too many unlock attempts — try again in a minute", http.StatusTooManyRequests)
		return
	}

	isForm := isFormRequest(r)

	var password, nextPath string
	var trust bool
	if isForm {
		if err := r.ParseForm(); err != nil {
			s.loginError(w, r, "invalid form", http.StatusBadRequest, "")
			return
		}
		if !validateCSRF(r) {
			s.log.Warn("login form rejected: csrf token missing or mismatch",
				zap.String("ip", ip))
			s.loginError(w, r, "session expired — please reload the login page", http.StatusForbidden, "")
			return
		}
		password = r.PostFormValue("password")
		trust = r.PostFormValue("trust") != ""
		nextPath = r.PostFormValue("next")
	} else {
		var body struct {
			Password string `json:"password"`
			Trust    bool   `json:"trust"`
			Next     string `json:"next,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		password = body.Password
		trust = body.Trust
		nextPath = body.Next
	}

	safeNext := ""
	if nextPath != "" {
		if isSafeNextPath(nextPath) {
			safeNext = nextPath
		} else {
			s.log.Warn("rejecting unsafe next path on login", zap.String("next", nextPath))
		}
	}

	expected := s.resolvePassphrase(r.Context())
	if expected != "" && subtle.ConstantTimeCompare([]byte(password), []byte(expected)) != 1 {
		s.loginError(w, r, "incorrect password", http.StatusUnauthorized, safeNext)
		return
	}

	token := s.sessions.issue()
	setSessionCookie(w, token)

	if trust && s.dbSessions != nil {
		ua := r.Header.Get("User-Agent")
		if devToken, err := s.dbSessions.issueDeviceToken(r.Context(), ip, ua); err == nil {
			setDeviceCookie(w, devToken)
		}
	}

	if isForm {
		target := safeNext
		if target == "" {
			target = "/hooks/webui"
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	}

	resp := map[string]string{"status": "ok"}
	if safeNext != "" {
		resp["next"] = safeNext
	}
	jsonOK(w, resp)
}

func (s *Server) loginError(w http.ResponseWriter, r *http.Request, msg string, code int, safeNext string) {
	if !isFormRequest(r) {
		jsonErr(w, msg, code)
		return
	}
	csrf, err := s.issueCSRFToken(w)
	if err != nil {
		s.log.Error("login error render: failed to issue csrf token", zap.Error(err))
		jsonErr(w, msg, code)
		return
	}
	body, err := renderLoginPage(s.loginTitle(safeNext), safeNext, csrf, msg)
	if err != nil {
		s.log.Error("login error render: template execute", zap.Error(err))
		jsonErr(w, msg, code)
		return
	}
	setLoginPageHeaders(w)
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Query().Get("next")
	if next != "" && !isSafeNextPath(next) {
		s.log.Warn("rejecting unsafe next on login page", zap.String("next", next))
		next = ""
	}
	csrf, err := s.issueCSRFToken(w)
	if err != nil {
		s.log.Error("login page: failed to issue csrf token", zap.Error(err))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	body, err := renderLoginPage(s.loginTitle(next), next, csrf, "")
	if err != nil {
		s.log.Error("login page: template execute", zap.Error(err))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	setLoginPageHeaders(w)
	_, _ = w.Write(body)
}

// isFormRequest returns true when the request body is a browser-style form
// submission (application/x-www-form-urlencoded or multipart/form-data).
func isFormRequest(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	return strings.HasPrefix(ct, "application/x-www-form-urlencoded") ||
		strings.HasPrefix(ct, "multipart/form-data")
}

// setLoginPageHeaders applies defence-in-depth headers on any response that
// renders the login form. Clickjacking prevention (XFO + frame-ancestors),
// no-referrer to keep the `next` path from leaking to upstream origins, and
// a CSP that allows only same-origin subresources plus inline styles.
func setLoginPageHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy",
		"default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'none'; "+
			"img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	h.Set("Referrer-Policy", "no-referrer")
}

const csrfCookie = "dicode_csrf"

// issueCSRFToken generates a fresh CSRF token, sets it as a same-site cookie,
// and returns the raw value for embedding in a form's hidden _csrf field.
// The double-submit pattern: the cookie travels only on same-origin requests
// (SameSite=Strict), so a cross-site POST cannot include the cookie — and
// without it validateCSRF rejects the request.
//
// The Secure attribute is set whenever TLS is configured (cfg.Server.TLSCert
// non-empty). Set it unconditionally and plain-HTTP localhost loses the
// cookie entirely, breaking the login flow; skip it and HTTPS deployments
// are vulnerable to cookie leak over any downgrade. Conditional on TLS
// config is the pragmatic middle ground.
func (s *Server) issueCSRFToken(w http.ResponseWriter) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.Server.TLSCertFile != "",
		SameSite: http.SameSiteStrictMode,
	})
	return token, nil
}

// validateCSRF enforces the double-submit cookie on a form POST. The cookie
// value (set by a prior /login GET) must match the form's _csrf field.
// Call only after r.ParseForm() has run. Constant-time compare.
func validateCSRF(r *http.Request) bool {
	c, err := r.Cookie(csrfCookie)
	if err != nil || c.Value == "" {
		return false
	}
	got := r.PostFormValue("_csrf")
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(got)) == 1
}

func (s *Server) loginTitle(next string) string {
	if next == "" {
		return "Sign in to dicode"
	}
	path := next
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	if !strings.HasPrefix(path, webhookPathPrefix) {
		return "Sign in to dicode"
	}
	slug := strings.TrimPrefix(path, webhookPathPrefix)
	if i := strings.Index(slug, "/"); i >= 0 {
		slug = slug[:i]
	}
	if slug == "" {
		return "Sign in to dicode"
	}
	for _, spec := range s.registry.All() {
		wp := spec.Trigger.Webhook
		if wp == "" {
			continue
		}
		if wp == webhookPathPrefix+slug || strings.HasPrefix(wp, webhookPathPrefix+slug+"/") {
			label := spec.Name
			if label == "" {
				label = spec.ID
			}
			if spec.Description != "" {
				return "Sign in to " + label + " — " + spec.Description
			}
			return "Sign in to " + label
		}
	}
	return "Sign in to dicode"
}

// loginPageTpl is compiled once at init time. html/template applies context-
// aware auto-escaping so every {{.X}} is safe in its surrounding markup:
// .Title goes into <title> (body text), .Err into body text, .Next and .CSRF
// into attribute values. Static analysers (CodeQL go/reflected-xss) recognise
// html/template as a sanitising sink.
var loginPageTpl = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.Title}}</title>
<style>` + loginCSS + `</style>
</head>
<body>
<main class="dc-login">
<form method="post" action="/api/auth/login" enctype="application/x-www-form-urlencoded">
<h1>{{.Title}}</h1>
{{if .Err}}<p class="dc-err" role="alert">{{.Err}}</p>{{end}}
<label>Password<input type="password" name="password" autocomplete="current-password" autofocus required></label>
<label class="dc-check"><input type="checkbox" name="trust" value="1">Trust this browser</label>
<input type="hidden" name="next" value="{{.Next}}">
<input type="hidden" name="_csrf" value="{{.CSRF}}">
<button type="submit">Sign in</button>
</form>
</main>
</body>
</html>`))

type loginPageData struct {
	Title string
	Next  string
	CSRF  string
	Err   string
}

// renderLoginPage produces the login form HTML with contextual auto-escaping
// via html/template. Returns nil + error only on template execution failure,
// which indicates a bug in the template itself (not a user-input issue).
func renderLoginPage(title, next, csrf, errMsg string) ([]byte, error) {
	var b strings.Builder
	if err := loginPageTpl.Execute(&b, loginPageData{
		Title: title, Next: next, CSRF: csrf, Err: errMsg,
	}); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

const loginCSS = `body{margin:0;font:16px/1.4 system-ui,sans-serif;background:#0f1115;color:#e6e8eb;display:grid;place-items:center;min-height:100vh}` +
	`.dc-login{width:100%;max-width:360px;padding:2rem}` +
	`.dc-login h1{font-size:1.25rem;margin:0 0 1.25rem;font-weight:600}` +
	`.dc-login form{display:flex;flex-direction:column;gap:0.75rem}` +
	`.dc-login label{display:flex;flex-direction:column;gap:0.25rem;font-size:0.85rem;color:#9aa1ab}` +
	`.dc-login input[type=password]{padding:0.6rem 0.7rem;border:1px solid #2a2f38;background:#181b21;color:#e6e8eb;border-radius:4px;font:inherit}` +
	`.dc-login input[type=password]:focus{outline:2px solid #3b82f6;border-color:transparent}` +
	`.dc-login .dc-check{flex-direction:row;align-items:center;gap:0.5rem;color:#cbd0d7}` +
	`.dc-login button{margin-top:0.5rem;padding:0.65rem;background:#3b82f6;border:0;color:#fff;font:inherit;font-weight:600;border-radius:4px;cursor:pointer}` +
	`.dc-login button:hover{background:#2563eb}` +
	`.dc-err{margin:0 0 0.5rem;padding:0.5rem 0.7rem;background:#3a1a1a;border:1px solid #6b2424;color:#fca5a5;border-radius:4px;font-size:0.85rem}`

func (s *Server) apiListSecrets(w http.ResponseWriter, r *http.Request) {
	if s.secretsMgr == nil {
		jsonErr(w, "secrets not configured", http.StatusServiceUnavailable)
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
	if s.secretsMgr == nil {
		jsonErr(w, "secrets not configured", http.StatusServiceUnavailable)
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
	if s.secretsMgr == nil {
		jsonErr(w, "secrets not configured", http.StatusServiceUnavailable)
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
	type configResponse struct {
		*config.Config
		RelayHookBaseURL string `json:"relay_hook_base_url,omitempty"`
	}
	resp := configResponse{Config: s.cfg}
	if s.relayClient != nil {
		resp.RelayHookBaseURL = s.relayClient.HookBaseURL()
	}
	jsonOK(w, resp)
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
	id := taskIDParam(r)
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
	id := taskIDParam(r)
	s.log.Info("run requested via API", zap.String("task", id))
	runID, err := s.engine.FireManual(r.Context(), id, nil)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonOK(w, map[string]string{"runId": runID})
}

func (s *Server) apiListRuns(w http.ResponseWriter, r *http.Request) {
	id := taskIDParam(r)
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

// apiSaveAISettings persists the ai.task config pointer. Validation mirrors
// the /api/ai/chat forward guard: the task must be registered AND have a
// webhook under /hooks/ — anything else would be saved only to fail every
// subsequent chat call with the same structured error.
func (s *Server) apiSaveAISettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Task string `json:"task"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, "bad request", http.StatusBadRequest)
		return
	}
	task := strings.TrimSpace(body.Task)
	if task == "" {
		jsonErr(w, "task id is required", http.StatusBadRequest)
		return
	}
	spec, ok := s.registry.Get(task)
	if !ok {
		jsonErr(w, "task not found: "+task, http.StatusBadRequest)
		return
	}
	if !strings.HasPrefix(spec.Trigger.Webhook, webhookPathPrefix) {
		jsonErr(w, "task must have a webhook trigger under "+webhookPathPrefix, http.StatusBadRequest)
		return
	}
	s.cfg.AI.Task = task
	if err := s.persistConfig(); err != nil {
		s.log.Warn("settings persist failed", zap.Error(err))
		jsonErr(w, "saved in memory but could not write file: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("ai settings updated", zap.String("task", task))
	jsonOK(w, map[string]string{"status": "ok"})
}

func (s *Server) apiSaveServerSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LogLevel string `json:"log_level"`
		Secret   string `json:"secret"`
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

	doc["log_level"] = s.cfg.LogLevel

	// Serialise ai.task only when it diverges from the default so the
	// generated file stays minimal — the default lives in applyDefaults
	// and users who never touch this setting shouldn't see a stray block.
	// Mirror the serverMap / sources / runtimes pattern: mutate just the
	// key we own and leave any sibling `ai.*` keys untouched, so a future
	// AI config knob a user has handwritten survives a Save round-trip.
	aiMap, _ := doc["ai"].(map[string]any)
	if s.cfg.AI.Task != "" && s.cfg.AI.Task != "buildin/dicodai" {
		if aiMap == nil {
			aiMap = map[string]any{}
		}
		aiMap["task"] = s.cfg.AI.Task
		doc["ai"] = aiMap
	} else if aiMap != nil {
		delete(aiMap, "task")
		if len(aiMap) == 0 {
			delete(doc, "ai")
		} else {
			doc["ai"] = aiMap
		}
	}

	serverMap, _ := doc["server"].(map[string]any)
	if serverMap == nil {
		serverMap = map[string]any{}
	}
	serverMap["port"] = s.cfg.Server.Port
	serverMap["secret"] = s.cfg.Server.Secret
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
