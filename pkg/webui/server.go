// Package webui serves the REST API and SPA dashboard.
package webui

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/dicode/dicode/internal/fsutil"
	"github.com/dicode/dicode/internal/pathguard"
	"github.com/dicode/dicode/pkg/approval"
	"github.com/dicode/dicode/pkg/audit"
	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	pkgruntime "github.com/dicode/dicode/pkg/runtime"
	denoruntime "github.com/dicode/dicode/pkg/runtime/deno"
	"github.com/dicode/dicode/pkg/secrets"
	gitSource "github.com/dicode/dicode/pkg/source/git"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/taskset"
	"github.com/dicode/dicode/pkg/tasktest"
	"github.com/dicode/dicode/pkg/trigger"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/httprate"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// SecretsManager is an alias for secrets.Manager kept for call-site clarity.
type SecretsManager = secrets.Manager

// allowedOverrideJSONFields lists the top-level keys accepted in a
// PATCH /api/tasks/{id}/overrides body. Mirrors the yaml tags on
// taskset.Overrides — keep in sync if Overrides gains fields.
var allowedOverrideJSONFields = map[string]bool{
	"enabled":     true,
	"name":        true,
	"description": true,
	"trigger":     true,
	"params":      true,
	"env":         true,
	"net":         true,
	"fs":          true,
	"timeout":     true,
	"retry":       true,
	"runtime":     true,
	"dicode":      true,
	"defaults":    true,
	"entries":     true,
}

// secureCookies returns true when the daemon is reachable over HTTPS, either
// because it terminates TLS itself (tls_cert + tls_key set) or because it sits
// behind a TLS-terminating reverse proxy (trust_proxy: true). When true, the
// Secure attribute is set on session and device cookies.
func secureCookies(cfg *config.Config) bool {
	return cfg.Server.TrustProxy || (cfg.Server.TLSCertFile != "" && cfg.Server.TLSKeyFile != "")
}

// newSessionManager creates an scs.SessionManager backed by SQLite via the
// given db.DB. When database is nil (tests without persistence) a plain
// in-memory map store is used as a fallback. secure sets the Secure flag on
// the session cookie — pass secureCookies(cfg) so the flag is set whenever the
// connection is HTTPS (direct TLS or a TLS-terminating reverse proxy).
func newSessionManager(database db.DB, secure bool) *scs.SessionManager {
	sm := scs.New()
	sm.Lifetime = sessionTTL
	sm.Cookie.Name = sessionCookie
	sm.Cookie.HttpOnly = true
	sm.Cookie.SameSite = http.SameSiteStrictMode
	sm.Cookie.Persist = true
	sm.Cookie.Secure = secure
	sm.Cookie.Path = "/"
	if database != nil {
		store := newSCSStore(database)
		store.startPurgeLoop()
		sm.Store = store
	}
	return sm
}

// unlockMaxAttempts and unlockWindow define the flat per-IP rate limit on the
// login endpoint. Enforced via go-chi/httprate middleware.
const (
	unlockMaxAttempts = 5
	unlockWindow      = time.Minute
)

// webhookPathPrefix is the URL prefix every webhook-triggered task's HTTP
// surface lives under. Anything outside it is infrastructure or SPA routing
// — the /api/ai/chat forward guard, webhookAuthGuard's public-path carve-out,
// the /hooks/* mux entry, and the slug-to-task resolver all share this.
// Keep the trailing slash to enforce boundary matching (TrimPrefix + HasPrefix
// semantics require it).
const webhookPathPrefix = "/hooks/"

//go:embed static
var staticFS embed.FS

//go:embed login
var loginEmbedFS embed.FS

// Version is the build-stamped version reported by GET /healthz. Set by
// pkg/daemon before calling webui.New(). Defaults to "dev" when unset.
var Version = "dev"

// Server is the HTTP server for the web UI and REST API.
type Server struct {
	registry *registry.Registry
	engine   *trigger.Engine
	// cfg is mutated by apiSaveConfigRaw, apiSaveAISettings, apiSaveServerSettings,
	// apiAddSource, apiRemoveSource, apiSaveRuntime, apiPatchTaskOverrides, and
	// concurrently read by every handler that touches configuration. Guard all
	// access with cfgMu — RLock for reads, Lock for writes. cfgPath is set once
	// at construction and is read without locking.
	cfgMu              sync.RWMutex
	cfg                *config.Config
	cfgPath            string               // path to dicode.yaml; empty in tests
	secretsMgr         SecretsManager       // nil if local provider not configured
	reconciler         *registry.Reconciler // nil if not wired
	sourceMgr          *SourceManager       // nil if not wired
	dataDir            string               // ~/.dicode or cfg.DataDir
	gateway            *ipc.Gateway
	db                 db.DB
	managedRuntimes    []pkgruntime.ManagedRuntime
	sm                 *scs.SessionManager    // short-lived browser sessions (scs/v2)
	scsStore           *scsStore              // underlying SQLite store for sm; nil in DB-less tests
	dbSessions         *dbSessionStore        // persistent trusted-device tokens
	apiKeys            *apiKeyStore           // MCP / programmatic API keys
	mcpTokenMinter     *mcpTokenMinter        // ephemeral per-run MCP token minter over apiKeys; nil in tests (no database)
	authoringSessions  *authoringSessionStore // AI-first authoring sessions
	passphraseStore    *passphraseStore       // auth passphrase persisted in DB
	cachedPassphrase   string                 // in-memory cache of stored DB value (bcrypt hash, or legacy plaintext during migration); invalidated on change
	cachedPassphraseMu sync.RWMutex           // guards cachedPassphrase
	migrateGroup       passphraseMigrator     // collapses concurrent legacy-passphrase migrations to one bcrypt+write
	logs               *LogBroadcaster
	ws                 *WSHub
	log                *zap.Logger
	port               int
	srv                *http.Server

	// replayer fires new runs from persisted inputs. Nil when input persistence
	// is disabled (SetReplayer not called); the /api/runs/{runID}/replay
	// endpoint returns 503 in that case.
	replayer *registry.Replayer

	// resumer spawns the continuation run for a suspended run (SetResumer).
	// Nil when not wired; the /api/runs/{runID}/resume endpoint returns 503.
	resumer Resumer

	// testGuard vetoes POST /api/tasks/{id}/test for a given task ID. The
	// approval gate wires its FireGuard here: a pending (unapproved) task's
	// test file runs with full host permissions, so it must be refused
	// exactly like a fire. Nil means allow.
	testGuard func(taskID string) error

	// approvalGate is the trust-on-change gate's approve/pending surface
	// (SetApprovalGate). Nil disables the pending flag and the approve
	// endpoints. approvalTokens persists single-use approve-link tokens
	// (SetApprovalTokenStore); nil disables the /approve/{token} link flow.
	approvalGate   ApprovalGate
	approvalTokens *approval.TokenStore
	// audit records denied-auth events and serves GET /api/audit (#45).
	// Nil when no database is wired (tests); all emission is nil-safe.
	audit *audit.Store
}

// SetReplayer wires a Replayer for the POST /api/runs/{runID}/replay
// endpoint. Pass nil to disable (the endpoint will return 503).
func (s *Server) SetReplayer(r *registry.Replayer) { s.replayer = r }

// SetTestGuard installs the approval gate's veto for POST /api/tasks/{id}/test.
// A non-nil error from the guard refuses the test run with 409. nil allows
// everything. Call after New and before Start.
func (s *Server) SetTestGuard(g func(taskID string) error) { s.testGuard = g }

// MintAPIKey implements ipc.APIKeyMinter. Generates a new API key with
// the given name and returns the raw value (which is shown once and
// must be captured by the caller). Used by the CLI for `dicode mcp
// install` to create a key for Claude Code's MCP config without
// requiring the operator to mint one in the dashboard manually.
func (s *Server) MintAPIKey(ctx context.Context, name string) (ipc.APIKeyMintResult, error) {
	raw, info, err := s.apiKeys.generate(ctx, name)
	if err != nil {
		return ipc.APIKeyMintResult{}, err
	}
	return ipc.APIKeyMintResult{
		Key:       raw,
		ID:        info.ID,
		Name:      info.Name,
		Prefix:    info.Prefix,
		CreatedAt: info.CreatedAt.Unix(),
	}, nil
}

// RevokeAPIKeyByName implements ipc.APIKeyMinter. Idempotent: returns
// nil even when no key matched (the dashboard may have revoked it
// already).
func (s *Server) RevokeAPIKeyByName(ctx context.Context, name string) error {
	return s.apiKeys.revokeByName(ctx, name)
}

// MCPTokenMinter returns the ephemeral per-run MCP token minter for wiring
// into the Deno/Python runtimes via their SetMCPTokenMinter. Returns a true
// nil pkgruntime.MCPTokenMinter (not a nil-valued non-nil interface) when no
// database is configured (tests) — callers can compare directly against nil.
func (s *Server) MCPTokenMinter() pkgruntime.MCPTokenMinter {
	if s.mcpTokenMinter == nil {
		return nil
	}
	return s.mcpTokenMinter
}

// SweepEphemeralMCPTokens revokes every ephemeral per-run MCP API key left
// over from a previous daemon session. Call once at startup before any new
// run can mint one: a run that was in flight when the daemon last stopped
// leaves its token orphaned, since nothing will ever call Revoke for it.
func (s *Server) SweepEphemeralMCPTokens(ctx context.Context) error {
	if s.apiKeys == nil {
		return nil
	}
	return s.apiKeys.revokeByNamePrefix(ctx, ephemeralKeyPrefix)
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
	sm := newSessionManager(database, secureCookiesFor(cfg))

	wsHub := NewWSHub(log, wsOriginPatterns(cfg.Server.AllowedOrigins))

	var dbs *dbSessionStore
	var aks *apiKeyStore
	var mtm *mcpTokenMinter
	var ps *passphraseStore
	var store *scsStore
	var as *authoringSessionStore
	if database != nil {
		dbs = newDBSessionStore(database, log)
		aks = newAPIKeyStore(database)
		mtm = newMCPTokenMinter(aks)
		ps = newPassphraseStore(database)
		store = sm.Store.(*scsStore)
		as = newAuthoringSessionStore(database)
	}

	s := &Server{
		registry:          r,
		engine:            eng,
		cfg:               cfg,
		cfgPath:           cfgPath,
		secretsMgr:        secretsMgr,
		reconciler:        rec,
		sourceMgr:         sourceMgr,
		dataDir:           dataDir,
		sm:                sm,
		scsStore:          store,
		dbSessions:        dbs,
		apiKeys:           aks,
		mcpTokenMinter:    mtm,
		authoringSessions: as,
		passphraseStore:   ps,
		logs:              logs,
		ws:                wsHub,
		log:               log,
		port:              port,
		gateway:           gateway,
		db:                database,
		audit:             audit.NewStore(database),
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
	eng.AddRunFinishedHook(func(taskID, runID, status, triggerSource string, durationMs int64) {
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

	// Wire reconciler hooks → broadcast tasks:changed when tasks are added/removed.
	// Chain with the existing callbacks (already wired to the trigger engine in main).
	if rec != nil {
		prev := rec.OnRegister
		rec.OnRegister = func(k task.Kinded) {
			if prev != nil {
				prev(k)
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

	// Bind the cfg mutex into the SourceManager so its cfg reads serialise
	// with our writers. SourceManager is constructed in daemon initialisation
	// before the Server, hence the late-bind setter.
	if sourceMgr != nil {
		sourceMgr.BindCfgMutex(&s.cfgMu)
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
	r.Use(s.sm.LoadAndSave) // scs session middleware — must wrap all handlers

	// Auth endpoints — always public (login flow must be reachable without session).
	//
	// CSRF for form POSTs is enforced via Origin-header check in apiSecretsUnlock
	// (validateOrigin). JSON POSTs follow a different threat model: same-origin
	// fetch with SameSite=Strict session cookie — no form token needed.
	r.Get("/login", s.serveLoginPage)
	r.Get("/login/style.css", serveLoginFile("style.css", "text/css; charset=utf-8"))
	r.Get("/login/login.js", serveLoginFile("login.js", "text/javascript; charset=utf-8"))
	r.Get("/api/login/context", s.apiLoginContext)
	// Per-IP rate limit on the login POST (5 req/min). Skipped when
	// DICODE_DISABLE_UNLOCK_LIMITER=1 so e2e tests can rapid-fire logins.
	r.Group(func(rl chi.Router) {
		if os.Getenv("DICODE_DISABLE_UNLOCK_LIMITER") != "1" {
			rl.Use(httprate.Limit(unlockMaxAttempts, unlockWindow,
				httprate.WithKeyFuncs(func(r *http.Request) (string, error) {
					s.cfgMu.RLock()
					trustProxy := s.cfg.Server.TrustProxy
					s.cfgMu.RUnlock()
					return clientIP(r, trustProxy), nil
				}),
			))
		}
		rl.Post("/api/auth/login", s.apiSecretsUnlock)
	})
	r.Post("/api/auth/refresh", s.apiAuthRefresh)

	// /healthz: unauthenticated liveness probe. Used by Docker smoke tests,
	// Kubernetes liveness/readiness probes, and uptime monitors. Must remain
	// outside the auth-required group.
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":  "ok",
			"version": Version,
		})
	})

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
			// Generic overrides patch endpoint. Wildcard route (rather than
			// {id}/overrides) accepts namespaced task IDs containing slashes
			// like "buildin/relay-client" without requiring the caller to
			// percent-encode the separator.
			r.Patch("/tasks/*", s.apiPatchTaskOverrides)

			// /api/runs (collection): supports filtering by parent or by
			// (group + task), per #116. /api/tasks/{id}/runs above remains
			// the canonical "list a task's runs" endpoint.
			r.Get("/runs", s.apiQueryRuns)

			// Security audit log (#45) — protected by requireAuth above.
			r.Get("/audit", s.apiQueryAudit)
			r.Get("/runs/{runID}", s.apiGetRun)
			r.Get("/runs/{runID}/logs", s.apiGetLogs)
			// Aggregated logs across a run's whole root_run_id group (#569) —
			// e.g. every turn of a suspend/resume conversation, interleaved by ts.
			r.Get("/runs/{runID}/group-logs", s.apiGetGroupLogs)
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
			r.Delete("/settings/sources/{name}", s.apiRemoveSource)
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

			// AI-first task authoring
			r.Post("/task/create", s.apiTaskCreate)
			r.Post("/task/edit", s.apiTaskEdit)
			r.Post("/task/save", s.apiTaskSave)
			r.Post("/task/cancel", s.apiTaskCancel)

			// Managed runtime lifecycle
			r.Get("/runtimes", s.apiListRuntimes)
			r.Post("/runtimes/{name}/install", s.apiInstallRuntime)
			r.Delete("/runtimes/{name}", s.apiRemoveRuntime)
		})

		// Redirect root and unmatched GET routes to the webui webhook task.
		r.Get("/*", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/hooks/webui", http.StatusFound)
		})
	})

	// MCP endpoint — Bearer API key only (#698). Mounted outside the requireAuth
	// group so Bearer-only clients are not rejected before the key is checked.
	// No session-cookie fallback: nothing in the WebUI actually calls /mcp from
	// the browser (the Security page's "copy claude mcp add command" only
	// builds a CLI string; the AI chat panel goes through /api/ai/chat), and
	// allowing a plain authenticated browser session to reach /mcp would widen
	// the credential surface beyond the documented API-key-only policy (an XSS
	// or CSRF on another same-origin page would get MCP access without ever
	// holding an API key). The actual JSON-RPC dispatch lives in the
	// buildin/mcp dicode task (dicode-buildin's mcp/task.ts).
	// MCP is a *bool pointer; nil = default enabled once applyDefaults has filled it in.
	s.cfgMu.RLock()
	mcpEnabled := s.cfg == nil || s.cfg.Server.MCP == nil || *s.cfg.Server.MCP
	s.cfgMu.RUnlock()
	if mcpEnabled {
		r.With(s.requireAPIKey).HandleFunc("/mcp", s.handleMCP)
	}

	// Task test endpoint (#208) — API-key gated, mounted OUTSIDE the
	// session-auth group so external automation (CI scripts, MCP clients)
	// can drive the test harness with a Bearer token without first
	// establishing a browser session. The requireAPIKey middleware is a
	// no-op when server.auth is false, so the unauthenticated dev mode
	// continues to work the same way as before.
	r.With(s.requireAPIKey).Post("/api/tasks/{id}/test", s.apiTestTask)

	// Commit-push endpoint — API-key gated, mounted outside the session-auth
	// group so IPC tasks and CI scripts can call it with a Bearer token.
	// Ephemeral per-run tokens are denied: an agent must not push the source
	// changes it just authored (see requireNonEphemeralAPIKey).
	r.With(s.requireNonEphemeralAPIKey).Post("/api/sources/{name}/commit-push", s.apiCommitPush)

	// Replay endpoint — session cookie (WebUI replay button) or Bearer API key
	// (CLI / auto-fix scripts). Mounted outside the session-only group so
	// machine callers without cookies still work. An ephemeral per-run token
	// is denied: it must not re-drive arbitrary runs across the fleet.
	r.With(s.requireSessionOrNonEphemeralAPIKey).Post("/api/runs/{runID}/replay", s.apiReplayRun)

	// Resume endpoint (#95) — a suspended run's form submission. Same auth
	// posture as replay: session cookie (WebUI form) or non-ephemeral Bearer
	// key; the driving CLI uses its own managed key, so denying the ephemeral
	// per-run token here costs nothing and walls agents off from resuming
	// other runs with attacker-chosen input. The raw resume_token is resolved
	// server-side from the run, never trusted from the client.
	r.With(s.requireSessionOrNonEphemeralAPIKey).Post("/api/runs/{runID}/resume", s.apiResumeRun)

	// Approval-gate approve endpoint (#398) — session cookie (WebUI approve
	// button) or Bearer API key, but NOT an ephemeral per-run token: the
	// approval gate's whole purpose is that a human, not the authoring agent,
	// clears a changed task (see requireSessionOrNonEphemeralAPIKey).
	r.With(s.requireSessionOrNonEphemeralAPIKey).Post("/api/tasks/{id}/approve", s.apiApproveTask)

	// Same auth group as approve: an operator reviewing a task's end state
	// before clicking Approve needs the same trust boundary as approving it.
	r.With(s.requireSessionOrNonEphemeralAPIKey).Get("/api/tasks/{id}/pending-state", s.apiApprovalPendingState)

	// Tokenized approve link (#398) — the single-use token in the URL is the
	// auth, so these stay outside the session groups. GET renders a confirm
	// page without consuming the token (link prefetchers must not approve);
	// POST redeems it.
	r.Get("/approve/{token}", s.handleApproveLinkPage)
	r.Post("/approve/{token}", s.handleApproveLinkRedeem)

	return r
}

// Start listens on the configured port until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	// Ensure an auth passphrase exists before accepting any connections.
	// Auto-generates and prints one if server.auth is true and none is configured.
	if err := s.ensurePassphrase(ctx); err != nil {
		return fmt.Errorf("ensure auth passphrase: %w", err)
	}

	s.startAuthoringPurgeLoop(ctx)

	s.srv = &http.Server{
		Addr:              s.bindAddr(),
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
		// WriteTimeout is 0: WebSocket and SSE endpoints write indefinitely.
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutCtx)
	}()
	s.cfgMu.RLock()
	certFile := s.cfg.Server.TLSCertFile
	keyFile := s.cfg.Server.TLSKeyFile
	s.cfgMu.RUnlock()
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

// bindAddr returns the address the server should bind to.
// When auth is disabled, restrict to loopback to prevent accidental public exposure.
func (s *Server) bindAddr() string {
	s.cfgMu.RLock()
	auth := s.cfg.Server.Auth
	s.cfgMu.RUnlock()
	if !auth {
		return fmt.Sprintf("127.0.0.1:%d", s.port)
	}
	return fmt.Sprintf(":%d", s.port)
}

// secureCookies reports whether auth cookies should carry the Secure flag.
// True whenever the connection is HTTPS: either the daemon terminates TLS
// itself (tls_cert/tls_key set) or sits behind a TLS-terminating proxy
// (trust_proxy). Caller must hold no lock; this acquires cfgMu.
func (s *Server) secureCookies() bool {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return secureCookiesFor(s.cfg)
}

func secureCookiesFor(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	return cfg.Server.TrustProxy || (cfg.Server.TLSCertFile != "" && cfg.Server.TLSKeyFile != "")
}

// isAllowedGitScheme returns true when the URL uses a scheme valid for a git remote.
// Rejects file://, ftp://, data:, and other schemes that could trigger local-file
// reads or SSRF against non-git services. "git" is excluded (#486): go-git dials
// it through a native git-protocol transport with a hardcoded, unguarded
// net.Dial, so a git:// remote added here (via apiAddSource or previewed via
// apiListGitBranches) would get zero SSRF host validation — unlike http/https/ssh.
//
// Delegates to taskset.IsAllowedRefScheme rather than reimplementing the
// scheme switch: an earlier independent copy here had already drifted from
// it (it rejected the SCP-shorthand form, e.g. git@github.com:org/repo.git,
// that dicode.yaml's ref.url accepts), which is exactly the kind of gap a
// second, hand-maintained allowlist invites.
func isAllowedGitScheme(rawURL string) bool {
	return taskset.IsAllowedRefScheme(rawURL)
}

// wsOriginPatterns converts full-URL origin entries (e.g. "https://example.com")
// to the host-only patterns that coder/websocket.AcceptOptions.OriginPatterns expects.
func wsOriginPatterns(origins []string) []string {
	out := make([]string, 0, len(origins))
	for _, o := range origins {
		u, err := url.Parse(o)
		if err != nil || u.Host == "" {
			out = append(out, o) // pass through unchanged if not a valid URL
			continue
		}
		out = append(out, u.Host)
	}
	return out
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
	// A suspended run has produced neither output nor a return value, so the
	// checks below would 404 it. It is not finished — it is waiting on input.
	// Send the caller to the run page, which renders the resume form. This is
	// where a browser lands after a webhook task's form POST suspends.
	if run.Status == registry.StatusSuspended {
		http.Redirect(w, r, "/hooks/webui/runs/"+runID, http.StatusSeeOther)
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
	jsonOK(w, map[string]string{"content": string(redactServerSecret(b))})
}

// redactServerSecret removes the server.secret field from raw YAML bytes.
// It uses yaml.Node to parse so that comments and field ordering are preserved,
// and so all valid YAML forms of the field (plain scalar, block scalar |/>,
// flow-style inline mapping) are handled correctly.
func redactServerSecret(b []byte) []byte {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return b // not valid YAML; return as-is (no leak risk)
	}
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		if root := doc.Content[0]; root.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(root.Content); i += 2 {
				if root.Content[i].Value == "server" {
					serverNode := root.Content[i+1]
					if serverNode.Kind == yaml.MappingNode {
						// Remove the secret key-value pair.  Any trailing comment on the
						// secret key node is re-attached to the preceding value node (or
						// to the server mapping's FootComment) so it is not lost.
						newContent := make([]*yaml.Node, 0, len(serverNode.Content))
						for j := 0; j+1 < len(serverNode.Content); j += 2 {
							keyNode := serverNode.Content[j]
							if keyNode.Value == "secret" {
								if fc := keyNode.FootComment; fc != "" {
									if len(newContent) >= 2 {
										prev := newContent[len(newContent)-1]
										if prev.FootComment != "" {
											prev.FootComment += "\n" + fc
										} else {
											prev.FootComment = fc
										}
									} else {
										serverNode.FootComment = fc
									}
								}
								continue
							}
							newContent = append(newContent, keyNode, serverNode.Content[j+1])
						}
						serverNode.Content = newContent
					}
					break
				}
			}
		}
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return b
	}
	return out
}

// decodeBody populates v from a JSON request body when the Content-Type is
// application/json; for any other content type it invokes fromForm, which
// reads the equivalent form values into the same destination. The JSON
// decode error is returned for the caller to surface — call sites use
// different 400 bodies, preserved verbatim from before this helper existed.
func decodeBody(r *http.Request, v any, fromForm func()) error {
	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "application/json") {
		return json.NewDecoder(r.Body).Decode(v)
	}
	fromForm()
	return nil
}

// readContentField extracts the editor payload from either a JSON body
// ({"content": …}) or the "content" form value. ok=false means a JSON decode
// error was already written as a 400 response.
func readContentField(w http.ResponseWriter, r *http.Request) (string, bool) {
	var body struct {
		Content string `json:"content"`
	}
	if err := decodeBody(r, &body, func() { body.Content = r.FormValue("content") }); err != nil {
		jsonErr(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return "", false
	}
	return body.Content, true
}

// apiSaveConfigRaw validates and writes the raw config back to dicode.yaml.
// Protected by the main session via requireAuth.
func (s *Server) apiSaveConfigRaw(w http.ResponseWriter, r *http.Request) {
	if s.cfgPath == "" {
		jsonErr(w, "config file path not set", http.StatusBadRequest)
		return
	}

	// Support both JSON body and form value.
	content, ok := readContentField(w, r)
	if !ok {
		return
	}

	// Validate: must parse as valid YAML mapping.
	var check map[string]any
	if err := yaml.Unmarshal([]byte(content), &check); err != nil {
		jsonErr(w, "invalid YAML: "+err.Error(), http.StatusBadRequest)
		return
	}

	if err := config.AtomicWriteFile(s.cfgPath, []byte(content), 0600); err != nil {
		jsonErr(w, "write config: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Hot-reload config into memory (best-effort; server restart needed for port changes).
	if newCfg, err := config.Load(s.cfgPath); err == nil {
		s.cfgMu.Lock()
		s.cfg = newCfg
		if s.sourceMgr != nil {
			s.sourceMgr.SetCfg(newCfg)
		}
		s.cfgMu.Unlock()

		// Both override-mutation surfaces (this editor and PATCH
		// /api/tasks/{id}/overrides) must drive the same re-resolve →
		// EventUpdated → re-Admit pipeline. SetCfg alone only refreshes the
		// snapshot the REST API reads; the running taskset.Source keeps its
		// stale parentOverrides until restart, so a revoked permission
		// elevation would otherwise keep running with the broader grant.
		// Re-applying every entry's overrides joins the editor path to that
		// pipeline (SetParentOverrides coalesces no-op signals). Deleting a
		// whole source entry here does not tear down its running source —
		// source removal must go through DELETE /api/settings/sources/{name}.
		if s.sourceMgr != nil {
			for name, entry := range newCfg.Spec.Entries {
				if entry == nil {
					continue
				}
				if src, ok := s.sourceMgr.Get(name); ok {
					src.SetParentOverrides(entry.Overrides)
				}
			}
		}
	} else {
		s.log.Warn("config reload after raw save failed", zap.Error(err))
	}

	s.log.Info("config saved via code editor")
	jsonOK(w, map[string]string{"status": "ok"})
}

// allowedFiles restricts which files the editor API can read/write.
var allowedFiles = []string{
	"task.js", "task.ts", "task.py",
	"task.test.js", "task.test.ts",
	"Dockerfile",
	// Webhook UI files — editable via the built-in code editor.
	"index.html", "style.css", "script.js",
}

// canonicalTaskFile matches name against allowedFiles and returns the entry
// from that list rather than the caller's string. Everything downstream builds
// paths from the returned literal, so a request value never reaches the
// filesystem even if the comparison were later loosened.
func canonicalTaskFile(name string) (string, bool) {
	for _, allowed := range allowedFiles {
		if allowed == name {
			return allowed, true
		}
	}
	return "", false
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
//  3. Canonicalize symlinks on both sides and re-check containment.
//
// Step 3 is required because go-git materializes repo-committed symlinks as
// real on-disk links, so a source repo can plant tasks/foo/style.css -> a host
// path: the name passes every lexical check while the os.ReadFile/os.WriteFile
// downstream follows the link out of the task dir. Spec.ScriptPath rejects a
// symlinked task script for the same reason, and Hash folds an in-dir symlink's
// target string rather than its content — so a file reached through a link is
// one the approval gate never sees change.
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
	within, err := pathguard.WithinResolved(absDir, joined)
	if err != nil {
		return "", fmt.Errorf("resolve task file: %w", err)
	}
	if !within {
		return "", fmt.Errorf("path escapes task dir")
	}
	return joined, nil
}

// taskOr404 looks up a kind: Task spec by ID, writing the shared
// "task not found" 404 envelope when the registry has no such task.
// ok=false means the response was already written.
func (s *Server) taskOr404(w http.ResponseWriter, id string) (*task.Spec, bool) {
	spec, ok := s.registry.Get(id)
	if !ok {
		jsonErr(w, "task not found", http.StatusNotFound)
		return nil, false
	}
	return spec, true
}

// resolveTaskFile is the shared apiGetFile/apiSaveFile preamble: extract the
// task ID + filename params, enforce the editable-file allowlist, resolve the
// task, and validate the filename stays inside the task dir. ok=false means
// an error response was already written.
func (s *Server) resolveTaskFile(w http.ResponseWriter, r *http.Request) (path, id, filename string, ok bool) {
	id = taskIDParam(r)
	filename, allowed := canonicalTaskFile(chi.URLParam(r, "filename"))
	if !allowed {
		jsonErr(w, "file not allowed", http.StatusBadRequest)
		return "", "", "", false
	}
	spec, ok := s.taskOr404(w, id)
	if !ok {
		return "", "", "", false
	}
	path, err := safeTaskFilePath(spec.TaskDir, filename)
	if err != nil {
		jsonErr(w, "invalid filename", http.StatusBadRequest)
		return "", "", "", false
	}
	return path, id, filename, true
}

func (s *Server) apiGetFile(w http.ResponseWriter, r *http.Request) {
	path, _, _, ok := s.resolveTaskFile(w, r)
	if !ok {
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
	path, id, filename, ok := s.resolveTaskFile(w, r)
	if !ok {
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
	spec, ok := s.taskOr404(w, id)
	if !ok {
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
	if err := decodeBody(r, &body, func() {
		// Fallback: form values
		body.Type = r.FormValue("type")
		body.Cron = r.FormValue("cron")
		body.Webhook = r.FormValue("webhook")
		body.From = r.FormValue("chain_from")
		body.On = r.FormValue("chain_on")
		body.Restart = r.FormValue("restart")
	}); err != nil {
		jsonErr(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
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
	s.cfgMu.RLock()
	trustProxy := s.cfg.Server.TrustProxy
	s.cfgMu.RUnlock()
	ip := clientIP(r, trustProxy)

	isForm := isFormRequest(r)

	var password, nextPath string
	var trust bool
	if isForm {
		// Validate Origin header to defend against cross-origin form submissions
		// (CSRF). A missing Origin is allowed for curl and legacy clients that
		// do not send it; modern browsers send Origin on cross-origin POSTs and
		// will be rejected here if they don't match.
		if !validateOrigin(r) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Redirect(w, r, "/login?err=1", http.StatusSeeOther)
			return
		}
		password = r.PostFormValue("password")
		trust = r.PostFormValue("trust") != ""
		nextPath = r.PostFormValue("next")
	} else {
		// Non-form path (JSON and other Content-Types). A cross-origin fetch
		// with Content-Type: text/plain is a "simple" CORS request (no
		// preflight) and Go's json.Decoder ignores Content-Type, so the JSON
		// decode path is reachable cross-origin without a preflight. Validate
		// the Origin header when it is present to close that vector.
		if r.Header.Get("Origin") != "" && !validateOrigin(r) {
			jsonErr(w, "forbidden", http.StatusForbidden)
			return
		}
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

	// Auth is enabled but no passphrase has been configured yet (bootstrap
	// state) — accept any password, mirroring the previous behaviour. The
	// /security UI will force one to be set as soon as the operator logs in.
	//
	// passphraseSourceUnknown means the DB read failed; this is NOT treated
	// as bootstrap (which would accept any password). Reject
	// the login with 503 so the operator can investigate the outage rather
	// than silently letting anyone in.
	src := s.passphraseSource(r.Context())
	if src == passphraseSourceUnknown {
		// DB/passphrase read failed — surface as 503, not as "Incorrect password".
		if isForm {
			http.Error(w, "service temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		jsonErr(w, "service temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if src != passphraseSourceNone {
		if !s.verifyPassphrase(r.Context(), password) {
			s.auditDenied(r, "incorrect password")
			if isForm {
				http.Redirect(w, r, loginErrURL(safeNext), http.StatusSeeOther)
				return
			}
			jsonErr(w, "incorrect password", http.StatusUnauthorized)
			return
		}
	}

	s.sm.Put(r.Context(), "authenticated", true)

	if trust && s.dbSessions != nil {
		ua := r.Header.Get("User-Agent")
		if devToken, err := s.dbSessions.issueDeviceToken(r.Context(), ip, ua); err == nil {
			setDeviceCookie(w, devToken, s.secureCookies())
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

// serveLoginPage serves the embedded static login page HTML.
func (s *Server) serveLoginPage(w http.ResponseWriter, r *http.Request) {
	data, err := loginEmbedFS.ReadFile("login/index.html")
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	setLoginPageHeaders(w)
	_, _ = w.Write(data)
}

// serveLoginFile returns a handler that serves a named static login asset
// (style.css, login.js) embedded in the binary.
func serveLoginFile(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := loginEmbedFS.ReadFile("login/" + name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(data)
	}
}

// apiLoginContext returns JSON with the contextual page title and whether a
// real passphrase gates this login. The static login page fetches this via a
// short JS snippet to avoid server-side HTML rendering.
//
// passphrase_required is false only when passphraseSource() reports "none" —
// in practice that means server.auth is false, since ensurePassphrase (called
// synchronously from Start(), before the HTTP listener ever accepts a
// connection) generates and persists a passphrase before any request can
// possibly reach this handler whenever auth is enabled. In that state
// apiSecretsUnlock accepts any password, so the static HTML's unconditional
// `<input required>` was misleading: it looked like a real credential gate
// with no way to tell the operator otherwise. passphraseSourceUnknown (a
// transient DB read failure) reports required=true — login is fail-closed
// (503) in that state, so the page must keep looking like a real gate rather
// than implying it's open.
func (s *Server) apiLoginContext(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Query().Get("next")
	if next != "" && !isSafeNextPath(next) {
		next = ""
	}
	jsonOK(w, map[string]any{
		"title":               s.loginTitle(next),
		"passphrase_required": s.passphraseSource(r.Context()) != passphraseSourceNone,
	})
}

// requestScheme returns the externally visible scheme for r. TLS state takes
// precedence; X-Forwarded-Proto covers the common case where TLS terminates at
// a trusted reverse proxy and the backend receives plain HTTP.
func requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		return strings.ToLower(proto)
	}
	return "http"
}

// validateOrigin checks the Origin header against the request scheme+host to
// defend against cross-origin submissions (CSRF). A missing Origin is allowed —
// curl and other non-browser clients omit it and are not subject to CSRF.
// "null" is explicitly rejected: browsers send it from sandboxed contexts where
// the same-origin property cannot be established.
// Browsers canonically omit default ports from the Origin serialisation
// (https→443, http→80); we strip those same defaults from r.Host before
// comparing so deployments behind a proxy that preserves an explicit-port
// Host header (e.g. "myapp:443") still accept legitimate same-origin requests.
func validateOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if origin == "null" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	// Scheme must match the externally visible scheme of the request. A page
	// served over http:// is a different origin from https:// even on the same
	// host, so Origin: http://myapp must be rejected for an HTTPS request.
	if u.Scheme != requestScheme(r) {
		return false
	}
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	// Browsers omit the default port from Origin. Strip the same default from
	// r.Host when the origin carries no port, so "myapp" matches "myapp:443".
	if u.Port() == "" {
		switch u.Scheme {
		case "https":
			host = strings.TrimSuffix(host, ":443")
		case "http":
			host = strings.TrimSuffix(host, ":80")
		}
	}
	return u.Host == host
}

// loginErrURL returns the URL to redirect to when a form login fails.
func loginErrURL(safeNext string) string {
	if safeNext == "" {
		return "/login?err=1"
	}
	return "/login?err=1&next=" + url.QueryEscape(safeNext)
}

// isFormRequest returns true when the request body is a browser-style form
// submission (application/x-www-form-urlencoded or multipart/form-data).
func isFormRequest(r *http.Request) bool {
	ct := r.Header.Get("Content-Type")
	return strings.HasPrefix(ct, "application/x-www-form-urlencoded") ||
		strings.HasPrefix(ct, "multipart/form-data")
}

// setLoginPageHeaders applies defence-in-depth headers on the login page
// response. Clickjacking prevention (XFO + frame-ancestors), a referrer policy
// that keeps the `next` path from leaking cross-origin while preserving the
// Origin header on same-origin POSTs (needed for validateOrigin), and a CSP
// that allows same-origin scripts and styles (the page loads /login/login.js
// and /login/style.css from the same origin).
func setLoginPageHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy",
		"default-src 'self'; style-src 'self'; script-src 'self'; "+
			"img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
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

	var body struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := decodeBody(r, &body, func() {
		body.Key = r.FormValue("key")
		body.Value = r.FormValue("value")
	}); err != nil {
		jsonErr(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	key, value := body.Key, body.Value

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
	// Read relay hook URL from the KV store before acquiring cfgMu — the
	// query doesn't touch s.cfg, so keeping it outside the read lock avoids
	// blocking writers across a SQLite round-trip.
	var relayHookBaseURL string
	if raw, _ := readKvJSON(r.Context(), s.db, "buildin/relay-client:status"); raw != nil {
		var st struct {
			HookBaseURL string `json:"hook_base_url"`
		}
		_ = json.Unmarshal(raw, &st)
		relayHookBaseURL = st.HookBaseURL
	}
	// Hold the read lock across JSON marshalling — jsonOK encodes synchronously
	// and configResponse embeds *config.Config, which means the encoder walks
	// the live cfg's nested maps/slices.
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	jsonOK(w, configResponse{Config: s.cfg, RelayHookBaseURL: relayHookBaseURL})
}

// TaskListItem is the shape returned by GET /api/tasks for a kind: Task
// entry. It embeds the spec (the WebUI relies on the flat Spec fields) and
// additively surfaces the task `kind` so the list view can badge pipelines.
type TaskListItem struct {
	*task.Spec
	// Kind is the task kind discriminator (task.KindTask). Always set so the
	// UI can distinguish tasks from pipelines without inspecting other fields.
	Kind          string `json:"kind"`
	TriggerLabel  string `json:"trigger_label"`
	LastRunID     string `json:"last_run_id,omitempty"`
	LastRunStatus string `json:"last_run_status,omitempty"`
	// PendingApproval flags a task held by the trust-on-change approval gate:
	// its triggers are not armed until an operator approves it.
	PendingApproval bool `json:"pending_approval,omitempty"`
	// LoadError is set when this task's most recent reload attempt failed to
	// parse/validate (#649): the row shown is the last successfully
	// registered version, but a newer edit is currently broken and its
	// triggers were never updated to match it.
	LoadError string `json:"load_error,omitempty"`
}

// PipelineListItem is the shape returned by GET /api/tasks for a kind:
// PipelineTask entry. It mirrors the kind: Task fields the list
// view reads (id/name/enabled/kind/trigger_label/last_run_*) without embedding
// *task.Spec, since a PipelineTask is a peer of Spec, not a Spec.
type PipelineListItem struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Enabled         bool   `json:"enabled"`
	Kind            string `json:"kind"`
	TriggerLabel    string `json:"trigger_label"`
	LastRunID       string `json:"last_run_id,omitempty"`
	LastRunStatus   string `json:"last_run_status,omitempty"`
	PendingApproval bool   `json:"pending_approval,omitempty"`
	LoadError       string `json:"load_error,omitempty"`
}

// FailedTaskListItem is a synthetic GET /api/tasks row for an entry that has
// never registered a good version — a task.yaml that failed to parse on its
// very first load — so there is no *task.Spec/*task.PipelineTask to embed
// (#649). Kept minimal: this is a status row, not a stand-in for the real
// task shape, so most list columns render as their zero value / "—".
type FailedTaskListItem struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Kind         string `json:"kind"`
	Enabled      bool   `json:"enabled"`
	TriggerLabel string `json:"trigger_label"`
	LoadError    string `json:"load_error"`
}

func (s *Server) apiListTasks(w http.ResponseWriter, r *http.Request) {
	kinded := s.registry.AllKinded()
	// failures merges the reconciler's own direct-load failures (plain
	// git/local sources) with every taskset source's resolve failures — both
	// feed the same task.LoadFailure sink type (#649). Consumed twice below:
	// first to flag rows for IDs that ARE registered (an older good version
	// stays visible, now with an error badge), then to synthesize rows for
	// IDs that have never registered at all.
	failures := s.registry.LoadFailures()
	if s.sourceMgr != nil {
		for id, f := range s.sourceMgr.LoadFailures() {
			failures[id] = f
		}
	}
	items := make([]any, 0, len(kinded)+len(failures))
	seen := make(map[string]bool, len(kinded))
	for _, k := range kinded {
		// Last-run lookup is identical across kinds.
		var lastRunID, lastRunStatus string
		if runs, err := s.registry.ListRuns(r.Context(), k.TaskID(), 1); err == nil && len(runs) > 0 {
			lastRunID = runs[0].ID
			lastRunStatus = runs[0].Status
		}
		// Crash-loop override (#458): a crash-looping daemon's latest run is
		// intermittently a transient "running" (spawn-before-crash window).
		// Surface the loop state so the list never shows a hard-failing
		// daemon as healthy. Matches the CLI's cli.list derivation.
		if s.engine.IsCrashLooping(k.TaskID()) {
			lastRunStatus = string(trigger.DaemonCrashLooping)
		}
		pendingApproval := s.taskPendingApproval(k.TaskID())
		seen[k.TaskID()] = true
		loadErr := failures[k.TaskID()].Error
		switch v := k.(type) {
		case *task.Spec:
			items = append(items, TaskListItem{
				Spec:            v,
				Kind:            task.KindTask,
				TriggerLabel:    triggerLabel(v.Trigger),
				LastRunID:       lastRunID,
				LastRunStatus:   lastRunStatus,
				PendingApproval: pendingApproval,
				LoadError:       loadErr,
			})
		case *task.PipelineTask:
			items = append(items, PipelineListItem{
				ID:              v.ID,
				Name:            v.Name,
				Enabled:         v.Enabled,
				Kind:            task.KindPipelineTask,
				TriggerLabel:    pipelineTriggerLabel(v),
				LastRunID:       lastRunID,
				LastRunStatus:   lastRunStatus,
				PendingApproval: pendingApproval,
				LoadError:       loadErr,
			})
		default:
			// Forward-compat safety net: the registry only ever holds
			// *task.Spec / *task.PipelineTask today, so this branch is
			// effectively unreachable. We surface an unknown kind rather than
			// dropping it silently. PipelineListItem is reused only because its
			// field set matches what the list view reads; the kind it carries
			// is whatever KindOf() returns, which the JS badge won't recognize.
			// Give it a parity TriggerLabel ("—") so the list renders sanely
			// instead of falling back to a misleading "manual".
			items = append(items, PipelineListItem{
				ID:              k.TaskID(),
				Name:            k.TaskID(),
				Enabled:         k.IsEnabled(),
				Kind:            k.KindOf(),
				TriggerLabel:    "—",
				LastRunID:       lastRunID,
				LastRunStatus:   lastRunStatus,
				PendingApproval: pendingApproval,
				LoadError:       loadErr,
			})
		}
	}
	// Entries that have never registered a good version (a task.yaml that
	// failed to parse on its very first load) have no *task.Spec/*Kinded to
	// merge onto — synthesize a minimal row instead of leaving them absent
	// from the list (#649). Sorted so the list order is deterministic.
	var failedIDs []string
	for id := range failures {
		if !seen[id] {
			failedIDs = append(failedIDs, id)
		}
	}
	sort.Strings(failedIDs)
	for _, id := range failedIDs {
		f := failures[id]
		items = append(items, FailedTaskListItem{
			ID:           id,
			Name:         id,
			Kind:         "LoadError",
			Enabled:      false,
			TriggerLabel: "—",
			LoadError:    f.Error,
		})
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

	// DaemonState surfaces the engine's lifecycle phase for daemon tasks.
	// Empty for non-daemon tasks so the WebUI can hide the row entirely.
	// See pkg/trigger.DaemonState for the canonical enum.
	// The "failed_after_preflight" and "crashed" values are distinct from
	// "stopped" so operators can tell "fireAsync broke" (#318) and "body
	// crashed without auto-restart" (#325) apart from "deliberately stopped";
	// "crashlooping" (#458) marks a daemon stuck in a spawn/crash/backoff
	// loop that would otherwise sample as "running"; "suspended" (#95) marks a
	// body parked awaiting user input.
	DaemonState string `json:"daemon_state,omitempty"`

	// PendingApproval flags a task held by the trust-on-change approval gate.
	PendingApproval bool `json:"pending_approval,omitempty"`
}

// PipelineDetail is the shape returned by GET /api/tasks/{id} for a kind:
// PipelineTask. It embeds the pipeline spec (kind, name, stages, …) and adds
// a trigger label plus — when the terminal stage resolves to a daemon Task —
// that stage's daemon lifecycle phase, mirroring the kind: Task detail's
// daemon_state contract.
type PipelineDetail struct {
	*task.PipelineTask
	TriggerLabel    string `json:"trigger_label"`
	DaemonState     string `json:"daemon_state,omitempty"`
	PendingApproval bool   `json:"pending_approval,omitempty"`
}

func (s *Server) apiGetTask(w http.ResponseWriter, r *http.Request) {
	id := taskIDParam(r)
	spec, ok := s.registry.Get(id)
	if !ok {
		// Not a kind: Task — it may be a kind: PipelineTask. A PipelineTask is
		// a peer of Spec (not a Spec), so it needs its own detail shape.
		if k, kok := s.registry.GetKinded(id); kok {
			if p, pok := k.(*task.PipelineTask); pok {
				s.writePipelineDetail(w, p)
				return
			}
		}
		jsonErr(w, "task not found", http.StatusNotFound)
		return
	}

	detail := TaskDetail{
		Spec:            spec,
		TriggerLabel:    triggerLabel(spec.Trigger),
		PendingApproval: s.taskPendingApproval(spec.ID),
	}
	// Daemon lifecycle state — empty for non-daemon tasks so the UI
	// doesn't render "stopped" labels on every cron job.
	if spec.Trigger.Daemon {
		detail.DaemonState = string(s.engine.DaemonState(spec.ID))
	}

	// Determine script file
	switch spec.Runtime {
	case task.RuntimeDocker, task.RuntimePodman:
		detail.ScriptFile = "Dockerfile"
	default:
		// Use the runtime's own script discovery (Spec.ScriptPath) so the UI
		// reports exactly the file the runtime would execute — ScriptPath
		// rejects symlinked script entries, and a hand-rolled existence loop
		// here previously disagreed with it.
		if p := spec.ScriptPath(); p != "" {
			detail.ScriptFile = filepath.Base(p)
		} else {
			detail.ScriptFile = "task.ts"
		}
		for _, name := range []string{"task.test.ts", "task.test.js"} {
			if fsutil.Exists(filepath.Join(spec.TaskDir, name)) {
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

// writePipelineDetail renders the GET /api/tasks/{id} response for a kind:
// PipelineTask. It surfaces the pipeline's stages and, when the terminal
// stage resolves to a daemon Task, that stage's daemon lifecycle phase — a
// pipeline is daemon-shaped iff its terminal stage is a daemon, so operators
// see the same lifecycle badge they'd see on the bare daemon task.
func (s *Server) writePipelineDetail(w http.ResponseWriter, p *task.PipelineTask) {
	detail := PipelineDetail{
		PipelineTask:    p,
		TriggerLabel:    pipelineTriggerLabel(p),
		PendingApproval: s.taskPendingApproval(p.ID),
	}
	if len(p.Stages) > 0 {
		terminalID := p.Stages[len(p.Stages)-1].Task
		// Only a kind: Task with trigger.daemon: true has a daemon lifecycle.
		if termSpec, ok := s.registry.Get(terminalID); ok && termSpec.Trigger.Daemon {
			detail.DaemonState = string(s.engine.DaemonState(terminalID))
		}
	}
	jsonOK(w, detail)
}

// runTaskMaxBodyBytes bounds apiRunTask's optional JSON body. Params are
// unbounded by shape, so the byte count is the only thing standing between an
// authenticated caller and a gigabyte of JSON.
const runTaskMaxBodyBytes = 64 * 1024

// runTaskRequest is the optional JSON body accepted by apiRunTask. An absent
// body, an empty object, or an absent params key all mean "fire with the
// task's declared defaults" — the shape every caller sent before params were
// accepted here.
type runTaskRequest struct {
	Params map[string]any `json:"params,omitempty"`
}

// apiRunTask fires a manual run, optionally with fire-time param overrides —
// the browser equivalent of `dicode run <task> key=value`.
//
// Supplied params are validated against the task's declared schema before a
// run row exists, so a typo or a missing required value comes back as a 422
// the operator can correct in place rather than as a run that fails preflight
// (params_invalid) two seconds later with no logs to show for it.
//
// A kind: PipelineTask declares no params of its own, so there is no schema to
// validate against; its values are passed through and must already be strings.
//
// Status codes:
//   - 200 — run created; body carries runId
//   - 400 — bad JSON, non-string param on a pipeline, or the engine refused the fire
//   - 422 — params failed the task's schema; body carries per-field detail
func (s *Server) apiRunTask(w http.ResponseWriter, r *http.Request) {
	id := taskIDParam(r)
	s.log.Info("run requested via API", zap.String("task", id))

	var req runTaskRequest
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, runTaskMaxBodyBytes)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil && err != io.EOF {
			jsonErr(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	params, ok := s.coerceRunParams(w, id, req.Params)
	if !ok {
		return
	}

	// Record the operator principal (the session's client IP — the best
	// identity available under single-passphrase auth) so the run_triggered
	// audit event (#45) can answer "who triggered this manual run".
	runID, err := s.engine.FireManualWithActor(r.Context(), id, params, clientIP(r, s.cfg.Server.TrustProxy))
	if err != nil {
		jsonErr(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonOK(w, map[string]string{"runId": runID})
}

// coerceRunParams turns a JSON params object into the string map the engine
// merges over a task's declared defaults, writing the error response itself
// and reporting false when it did. A nil result fires the task unchanged.
func (s *Server) coerceRunParams(w http.ResponseWriter, id string, raw map[string]any) (map[string]string, bool) {
	if len(raw) == 0 {
		return nil, true
	}
	spec, ok := s.registry.Get(id)
	if !ok {
		// Pipelines (and anything else without a param schema) take values
		// verbatim; a non-string would reach the stage as Go's %v rendering
		// of whatever JSON produced, so refuse it instead of guessing.
		out := make(map[string]string, len(raw))
		for k, v := range raw {
			str, isStr := v.(string)
			if !isStr {
				jsonErr(w, "param "+k+" must be a string for a task with no declared schema",
					http.StatusBadRequest)
				return nil, false
			}
			out[k] = str
		}
		return out, true
	}
	coerced, errs := task.ValidateParams(spec.Params, raw)
	if len(errs) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":  "invalid params",
			"fields": errs,
		})
		return nil, false
	}
	return coerced, true
}

// apiPatchTaskOverrides accepts an RFC 7396 JSON Merge Patch against the
// taskset.Overrides yaml fields for the named task. Today the toggle UI
// sends {"enabled": false}; future param/env/timeout UIs reuse the same
// endpoint without backend changes.
//
// Route shape: PATCH /api/tasks/* — the wildcard captures the namespaced
// task ID followed by /overrides (e.g. "buildin/relay-client/overrides").
//
// Status codes:
//   - 200 — patch applied, dicode.yaml updated
//   - 400 — bad JSON, unknown field, or missing task id
//   - 404 — task not registered
//   - 409 — config file mtime changed since stat (concurrent edit)
//   - 422 — ancestor source disabled (enabling a child of a disabled source)
//   - 500 — config write or stat failed
func (s *Server) apiPatchTaskOverrides(w http.ResponseWriter, r *http.Request) {
	// The wildcard route stores the captured remainder under chi param "*".
	// URL-decode (frontend may percent-encode the slashes via
	// encodeURIComponent), then strip the trailing /overrides to recover the
	// task ID itself.
	rest, err := url.PathUnescape(chi.URLParam(r, "*"))
	if err != nil {
		rest = chi.URLParam(r, "*")
	}
	if !strings.HasSuffix(rest, "/overrides") {
		jsonErr(w, "expected path .../overrides", http.StatusBadRequest)
		return
	}
	id := strings.TrimSuffix(rest, "/overrides")
	if id == "" {
		jsonErr(w, "missing task id", http.StatusBadRequest)
		return
	}

	if _, ok := s.taskOr404(w, id); !ok {
		return
	}

	source, _, ok := config.SplitTaskID(id)
	if !ok {
		jsonErr(w, "task id has no source separator", http.StatusBadRequest)
		return
	}

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024))
	if err != nil {
		jsonErr(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	var patch map[string]json.RawMessage
	if err := json.Unmarshal(raw, &patch); err != nil {
		jsonErr(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	for k := range patch {
		if !allowedOverrideJSONFields[k] {
			jsonErr(w, "unknown override field: "+k, http.StatusBadRequest)
			return
		}
	}

	// 422: ancestor source disabled. Surface the actionable error rather
	// than writing a useless override.
	if enabledRaw, ok := patch["enabled"]; ok {
		var enabled *bool
		if string(enabledRaw) != "null" {
			var b bool
			if err := json.Unmarshal(enabledRaw, &b); err != nil {
				jsonErr(w, "enabled must be bool or null", http.StatusBadRequest)
				return
			}
			enabled = &b
		}
		if enabled != nil && *enabled {
			// LiftEntryEnabled (called by config.applyDefaults during Load)
			// has already moved any top-level entry.Enabled shortcut into
			// entry.Overrides.Enabled, so that's the only field to check.
			s.cfgMu.RLock()
			ancestorDisabled := false
			if entry := s.cfg.Spec.Entries[source]; entry != nil &&
				entry.Overrides != nil && entry.Overrides.Enabled != nil && !*entry.Overrides.Enabled {
				ancestorDisabled = true
			}
			s.cfgMu.RUnlock()
			if ancestorDisabled {
				jsonErr(w, "source "+source+" is disabled — enable the source first",
					http.StatusUnprocessableEntity)
				return
			}
		}
	}

	fi, err := os.Stat(s.cfgPath)
	if err != nil {
		jsonErr(w, "stat config: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := config.MergeTaskOverride(s.cfgPath, id, raw, fi.ModTime()); err != nil {
		if errors.Is(err, config.ErrConcurrentModification) {
			jsonErr(w, "config file modified externally", http.StatusConflict)
			return
		}
		jsonErr(w, "write override: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Reload dicode.yaml into memory so the running daemon reflects the
	// change without a restart. cfgMu serialises this with concurrent
	// readers (apiGetConfig, etc.) and other writers.
	updated, loadErr := config.Load(s.cfgPath)
	if loadErr != nil {
		s.log.Warn("config reload after override patch failed",
			zap.String("task", id), zap.Error(loadErr))
	} else {
		s.cfgMu.Lock()
		s.cfg.Spec = updated.Spec
		s.cfgMu.Unlock()
		if s.sourceMgr != nil {
			if src, ok := s.sourceMgr.Get(source); ok {
				if entry := updated.Spec.Entries[source]; entry != nil {
					src.SetParentOverrides(entry.Overrides)
				}
			}
		}
	}

	jsonOK(w, map[string]any{
		"id":        id,
		"overrides": patch,
	})
}

// Hardening caps for apiTestTask. testTaskMaxBodyBytes bounds the request
// payload so a misbehaving caller can't stream gigabytes of JSON; the cap is
// well above any realistic params payload but small enough to fail fast.
// testTaskMaxTimeout caps the runner subprocess lifetime so an authenticated
// caller can't pin a Deno process indefinitely with a giant timeout_s value.
//
// testTaskMaxTimeout is a var (not a const) so the test suite can override
// it to keep TimeoutCapClamps fast. Production code must never mutate it.
const testTaskMaxBodyBytes = 64 * 1024

var testTaskMaxTimeout = 5 * time.Minute

// testTaskRequest is the optional JSON body accepted by apiTestTask.
//
// Both fields are optional: an empty body, an empty {} object, or any
// missing field is treated as "use the task's defaults". Params are
// validated against the task.yaml `params` schema before the runner is
// invoked — schema mismatches return 422 with per-field detail.
type testTaskRequest struct {
	Params   map[string]any `json:"params,omitempty"`
	TimeoutS int            `json:"timeout_s,omitempty"`
}

// testTaskResponse is the wire shape returned on completion (200) regardless
// of whether the task itself passed. Field names follow #208's spec —
// snake_case here even though most other webui responses use camelCase,
// because this endpoint is designed for external automation callers (MCP
// clients, CI scripts) where snake_case is more idiomatic.
//
// Fields:
//   - status:       "passed" | "failed" | "errored"
//   - exit_code:    runner process exit code
//   - stdout:       combined stdout+stderr from the runner (named stdout for
//     compatibility with the issue spec; the runner intermixes streams).
//   - stderr:       always "" today — Deno's `deno test` interleaves streams
//     into a single buffer and we don't currently split them. Kept in the
//     response shape so future runtime backends (Python, Docker) can supply it.
//   - duration_ms:  wall-clock duration of the runner subprocess in ms
//   - run_id:       opaque correlation ID for this test invocation; surface in
//     logs / future audit trail. Not stored in the runs table because test
//     invocations are separate from the production run log.
type testTaskResponse struct {
	Status     string `json:"status"`
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMs int64  `json:"duration_ms"`
	RunID      string `json:"run_id"`
	// Diagnostic fields preserved from the underlying tasktest.Result so
	// callers can inspect counts / file path without re-parsing stdout.
	TestFile string `json:"test_file,omitempty"`
	Passed   int    `json:"passed"`
	Failed   int    `json:"failed"`
	Skipped  int    `json:"skipped"`
	Error    string `json:"error,omitempty"`
}

// apiTestTask runs a task's sibling test file via tasktest.RunByID and
// returns a structured result. Closes #208.
//
// Authentication: requireAPIKey (Bearer); mirrors /mcp's auth posture so
// the same API key works across both surfaces. A scoped ephemeral per-run
// MCP token (#567) additionally needs scope.TestTasks — i.e. the minting
// task must declare permissions.dicode.tasks_test: true — or the request is
// refused with 403 before the test file ever runs (#590: this REST endpoint
// is what the JSON-RPC test_task hint tool points MCP clients at, and it
// runs the sibling test file with full host permissions, so it must not be
// reachable by every scoped token regardless of declared capabilities).
// Unscoped operator/CLI/dashboard keys are unaffected.
//
// Body (optional): {"params": {...}, "timeout_s": int}. Supplied params are
// validated against the task's declared schema (422 on mismatch, with
// per-field detail); requiredness is not enforced, since a test file mocks
// its own params.
// timeout_s caps the runner subprocess lifetime; on expiry the handler
// returns 408 with whatever output was captured before cancellation.
//
// Status codes:
//   - 200 — runner completed (regardless of test pass/fail; see status field)
//   - 401 — bad/missing API key (handled upstream by requireAPIKey)
//   - 403 — scoped ephemeral token lacks permissions.dicode.tasks_test
//   - 404 — task ID not registered
//   - 408 — runner timed out
//   - 409 — task is held pending approval (test code must not execute)
//   - 422 — params payload failed schema validation
func (s *Server) apiTestTask(w http.ResponseWriter, r *http.Request) {
	id := taskIDParam(r)

	// Scope check for ephemeral MCP tokens (#590). Mirrors the pattern in
	// handleMCP: only a Bearer-authenticated caller can carry a scope at
	// all, and only a non-nil scope actually restricts anything.
	scope, found := s.scopeForRequest(r)
	// !found: requireAPIKey upstream already validated this token —
	// this is a defensive no-op, not a second auth gate. scope == nil:
	// unscoped operator/CLI/dashboard key — full access, proceed
	// unrestricted exactly as before this change.
	if found && scope != nil && !scope.TestTasks {
		jsonErr(w, "capability not granted: test_task", http.StatusForbidden)
		return
	}

	// Approval-gate veto: the sibling test file runs with full host
	// permissions, so a pending (unapproved) task must be refused here just
	// like a fire.
	if s.testGuard != nil {
		if err := s.testGuard(id); err != nil {
			jsonErr(w, err.Error(), http.StatusConflict)
			return
		}
	}

	// Body is optional. An empty body is the common case for "just run the
	// tests with defaults" — guard the decode against a zero-length stream
	// so we don't return 422 on the happy path. Cap the body so a misbehaving
	// or malicious caller cannot stream gigabytes of JSON at the daemon
	// (req.Params is unbounded by shape — only the byte count protects us).
	// DisallowUnknownFields keeps the closed-schema posture symmetric with
	// the params validator: top-level typos surface as 400 rather than being
	// silently ignored.
	var req testTaskRequest
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, testTaskMaxBodyBytes)
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil && err != io.EOF {
			jsonErr(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	// Clamp timeout_s to a safe upper bound. Without this an authenticated
	// caller could pin a runner subprocess for arbitrarily long; the ceiling
	// is generous (5 minutes) so legitimate slow test suites still fit.
	// Negative values are silently treated as "use parent ctx" (timeout=0).
	timeout := time.Duration(req.TimeoutS) * time.Second
	if timeout > testTaskMaxTimeout {
		timeout = testTaskMaxTimeout
	}
	if timeout < 0 {
		timeout = 0
	}
	// TODO: wire `coerced` (the validated string-typed params)
	// into tasktest.Run so the runner actually receives them. Today the
	// validator gates the call but the params themselves never reach Deno
	// — see the testTaskRequest godoc.
	res, _, runErr := tasktest.RunByID(r.Context(), s.registry, id, req.Params, timeout)

	// Map tasktest's typed errors to HTTP status codes per #208 acceptance.
	switch {
	case errors.Is(runErr, tasktest.ErrTaskNotFound):
		jsonErr(w, fmt.Sprintf("task %q not found", id), http.StatusNotFound)
		return
	case errors.Is(runErr, tasktest.ErrTimeout):
		// Surface the partial result alongside the 408 so callers see what
		// the runner managed to capture before the deadline tripped. The
		// status code is still authoritative ("did this complete?"); the
		// body carries forensic detail.
		body := buildTestTaskResponse(res, runErr, randomRunID())
		body.Status = "timeout"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestTimeout)
		_ = json.NewEncoder(w).Encode(body)
		return
	}
	var paramsErr *tasktest.ErrParamsInvalid
	if errors.As(runErr, &paramsErr) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":  "invalid params",
			"fields": paramsErr.FieldErrors,
		})
		return
	}

	// Successful completion — including failed tests. Per #208, 200 means
	// "the runner completed and we have a verdict to return"; the verdict
	// itself lives in the body's `status` field.
	jsonOK(w, buildTestTaskResponse(res, runErr, randomRunID()))
}

// scopeForRequest extracts a Bearer token from r's Authorization header and
// resolves its stored MCP scope, if any. found mirrors apiKeyStore.scopeFor:
// false when there's no Bearer token, no key store is configured (database
// nil — auth features disabled, see New's doc comment), or the token doesn't
// validate; scope nil when found but unscoped (full access).
func (s *Server) scopeForRequest(r *http.Request) (scope *pkgruntime.MCPScope, found bool) {
	if s.apiKeys == nil {
		return nil, false
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		return nil, false
	}
	return s.apiKeys.scopeFor(r.Context(), token)
}

// buildTestTaskResponse maps a tasktest.Result + runErr into the HTTP wire
// shape. status is derived from exit code + parsed counts:
//   - "errored" if Result.Error non-empty or runErr non-nil and not a clean
//     non-zero exit (e.g. spawn failure, deno-not-installed)
//   - "failed"  if exitCode != 0 OR Failed > 0
//   - "passed"  otherwise
func buildTestTaskResponse(res tasktest.Result, runErr error, runID string) testTaskResponse {
	resp := testTaskResponse{
		ExitCode:   res.ExitCode,
		Stdout:     res.Output,
		Stderr:     "", // see godoc on testTaskResponse.Stderr
		DurationMs: res.Duration.Milliseconds(),
		RunID:      runID,
		TestFile:   res.TestFile,
		Passed:     res.Passed,
		Failed:     res.Failed,
		Skipped:    res.Skipped,
		Error:      res.Error,
	}
	switch {
	case res.Error != "" || (runErr != nil && res.ExitCode == 0):
		resp.Status = "errored"
		if resp.Error == "" && runErr != nil {
			resp.Error = runErr.Error()
		}
	case res.ExitCode != 0 || res.Failed > 0:
		resp.Status = "failed"
	default:
		resp.Status = "passed"
	}
	return resp
}

// randomRunID returns an opaque correlation ID for a test invocation. Not a
// registry run ID — test invocations are kept off the runs
// table so they don't pollute history dashboards.
func randomRunID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Extremely unlikely (crypto/rand on Linux). Falling back to a
		// timestamp-derived ID keeps the response shape valid.
		return fmt.Sprintf("ts-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// handleMCP is the public /mcp endpoint. The actual JSON-RPC dispatch lives
// in the buildin/mcp dicode task; this handler exists so the historical
// /mcp URL keeps working without forcing every MCP client to be reconfigured
// to /hooks/mcp. GET returns a small server-info doc (so a curl probe still
// succeeds the way the old Go MCP server did); POST rewrites the URL path
// and re-enters the trigger engine's webhook dispatch via the gateway.
//
// Every caller here has already cleared requireAPIKey (#698) — /mcp accepts
// a Bearer API key only, no session-cookie fallback.
//
// Before forwarding a POST, a Bearer-authenticated caller holding a scoped
// ephemeral per-run MCP token (#567) is checked against mcpScopeCheck, then
// passed through mcpScopeRewrite: the buildin/mcp task itself runs with the
// dicode permissions every tool it serves needs, so it cannot be relied on to
// self-restrict — enforcement has to happen here, before the request ever
// reaches it. Unscoped (operator/CLI/dashboard) API keys are unaffected and
// forward unchanged.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"name":     "dicode",
			"version":  "dev",
			"protocol": "mcp/2024-11-05",
		})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Every request past requireAPIKey carries a Bearer token; resolve its
	// MCP scope, if any.
	scope, found := s.scopeForRequest(r)
	// !found: the api-key middleware upstream (requireAPIKey)
	// already validated this token: this is a defensive no-op, not a
	// second auth gate. scope == nil: unscoped operator/CLI/dashboard
	// key — full access, forward unchanged.
	if found && scope != nil {
		// Cap the read like every other request body in this file (see
		// apiTestTask / testTaskMaxBodyBytes below): MCP JSON-RPC
		// envelopes are small, so 64KB is generous headroom while still
		// preventing a scoped caller from streaming an unbounded body at
		// the daemon before mcpScopeCheck even runs. A MaxBytesReader
		// overflow surfaces here as a plain io.ReadAll error, handled by
		// the same 400 branch as any other malformed read.
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, testTaskMaxBodyBytes))
		if err != nil {
			jsonErr(w, "failed to read request body", http.StatusBadRequest)
			return
		}
		if allowed, id, deniedMsg := mcpScopeCheck(scope, body); !allowed {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      id,
				"error": map[string]any{
					"code":    -32001,
					"message": deniedMsg,
				},
			})
			return
		}
		forwarded, ok := mcpScopeRewrite(scope, body)
		if !ok {
			jsonErr(w, "failed to bind request to its run", http.StatusInternalServerError)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(forwarded))
		// The rewrite changes the body's length; a stale Content-Length
		// would truncate it downstream.
		r.ContentLength = int64(len(forwarded))
	}

	r.URL.Path = "/hooks/mcp"
	s.gateway.ServeHTTP(w, r)
}

// decodeRPCEnvelope decodes body as a single JSON-RPC envelope, reporting
// false for anything that is not exactly one top-level JSON object.
//
// Every read of the body must go through here. mcpScopeCheck allows a body it
// cannot parse, deferring to the task's own parse error; mcpScopeRewrite
// rewrites one it can. Should the two ever disagree about what parses, a body
// the check waved through as malformed becomes a well-formed call to the
// rewrite, which re-encodes it into a clean call nothing gated. The decoders
// differ in exactly the way that matters: Unmarshal rejects trailing data,
// while a Decoder stops at the first value and ignores the rest.
//
// The trailing-data check below is what makes this stricter reading match
// Unmarshal's, and pkg/trigger/webhook.go parses the body with Unmarshal to
// build the task's input — so all three reads agree.
//
// Numbers are decoded with UseNumber so a large JSON-RPC id survives being
// re-encoded, and so a denial echoes back the id the client actually sent.
func decodeRPCEnvelope(body []byte) (map[string]any, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var raw map[string]any
	if err := dec.Decode(&raw); err != nil {
		return nil, false
	}
	if dec.More() {
		// Trailing data after the envelope.
		return nil, false
	}
	return raw, true
}

// mcpScopeCheck decides whether a scoped ephemeral MCP token (scope) may
// make the JSON-RPC call encoded in body, without performing any I/O — pure
// so it's cheap to table-test. scope is never nil here (callers only invoke
// this once they've already established the key is scoped); a zero-value
// *MCPScope correctly denies every tools/call.
//
//   - initialize / tools/list: always allowed (discovery, exercises no
//     dicode capability).
//   - tools/call list_tasks / get_task: requires scope.ListTasks.
//   - tools/call run_task: requires scope.RunTaskIDs to contain "*" or the
//     call's params.arguments["id"].
//   - tools/call list_sources: requires scope.ListSources.
//   - tools/call test_task: requires scope.TestTasks — the same flag
//     apiTestTask checks for the REST endpoint POST /api/tasks/{id}/test
//     (#590). Both surfaces run the task's sibling test file with full host
//     permissions, so they carry one gate between them.
//   - tools/call switch_dev_mode: requires scope.SetDevMode, and additionally
//     requires the token to carry a RunID — see mcpScopeRewrite for why the
//     call's own run_id argument is not trusted.
//   - tools/call for any other (unrecognized, or future) tool name: denied.
//     This is fail-closed: nothing ties this switch to
//     dicode-buildin's mcp/task.ts TOOLS/dispatchTool list, so a future tool
//     added there that exercises a real scoped capability must not silently
//     inherit full access just because this switch doesn't know its name
//     yet. Note this only affects a *scoped* caller — an unscoped caller
//     (scope == nil) never reaches mcpScopeCheck at all and still gets the
//     task's own "unknown tool" error unchanged.
//   - any other method: always allowed — let the task's own
//     "-32601 method not found" path handle it.
//   - a body whose top level isn't a JSON object at all (invalid JSON, or a
//     JSON array/scalar/etc): always allowed — let the task's own
//     "-32700 parse error" path handle it, no second error shape.
//
// The top-level decode is intentionally into a generic map rather than a
// typed struct: with a typed struct, a single sibling field failing to
// type-match (e.g. params.arguments sent as a string instead of an object)
// makes encoding/json return a non-nil error *after* it has already
// populated every other field it could, including method and params.name —
// and treating "any decode error" as "not JSON-RPC, allow" would then
// discard those correctly-decoded, security-relevant fields and bypass
// enforcement entirely. Decoding into map[string]any never fails that way;
// each field of interest is instead extracted defensively below via a
// type assertion that degrades to its zero value on a shape mismatch,
// rather than aborting the whole check.
//
// Returns allowed, the request's id (for echoing back in a denial), and a
// human-readable denial message (empty when allowed).
func mcpScopeCheck(scope *pkgruntime.MCPScope, body []byte) (allowed bool, id any, deniedMsg string) {
	raw, ok := decodeRPCEnvelope(body)
	if !ok {
		// Top level isn't a JSON object at all: genuinely not JSON-RPC.
		return true, nil, ""
	}
	id = raw["id"]

	method, _ := raw["method"].(string)
	switch method {
	case "tools/call":
		// fall through to the tool-name switch below.
	default:
		// initialize, tools/list, unknown methods, and anything else: no
		// scoped capability is exercised here, let the task handle it.
		return true, id, ""
	}

	params, _ := raw["params"].(map[string]any)
	name, _ := params["name"].(string)
	switch name {
	case "list_tasks", "get_task":
		if scope.ListTasks {
			return true, id, ""
		}
		return false, id, fmt.Sprintf("capability not granted: %s", name)
	case "run_task":
		// arguments failing to type-match as an object (e.g. sent as a
		// string or number) degrades to "no id available" rather than
		// aborting the check — the existing taskID == "" deny-by-default
		// branch below still closes the gap.
		arguments, _ := params["arguments"].(map[string]any)
		taskID, _ := arguments["id"].(string)
		for _, allowedID := range scope.RunTaskIDs {
			if allowedID == "*" || allowedID == taskID {
				return true, id, ""
			}
		}
		if taskID != "" {
			return false, id, fmt.Sprintf("capability not granted: %s for task %q", name, taskID)
		}
		return false, id, fmt.Sprintf("capability not granted: %s", name)
	case "list_sources":
		if scope.ListSources {
			return true, id, ""
		}
		return false, id, fmt.Sprintf("capability not granted: %s", name)
	case "test_task":
		if scope.TestTasks {
			return true, id, ""
		}
		return false, id, fmt.Sprintf("capability not granted: %s", name)
	case "switch_dev_mode":
		// A token with no RunID cannot be given a clone directory it owns,
		// and mcpScopeRewrite has nothing to bind the call to — refuse
		// rather than let the caller's own run_id through.
		if scope.SetDevMode && scope.RunID != "" {
			return true, id, ""
		}
		return false, id, fmt.Sprintf("capability not granted: %s", name)
	default:
		// Fail closed: an unrecognized tool name — including any future
		// tool added to dicode-buildin's mcp/task.ts that this switch hasn't
		// been taught about — is denied rather than silently inheriting
		// full access from a scoped token.
		return false, id, fmt.Sprintf("capability not granted: %s", name)
	}
}

// mcpScopeRewrite returns body with any caller-supplied run_id on a
// switch_dev_mode call replaced by the run the token was minted for, or body
// unchanged when the call is anything else.
//
// run_id names the per-session clone directory dev mode creates, and
// SetDevMode uses it again to find that directory on the way out. A caller
// that picks the name picks which session's clone it addresses — so the name
// is not the caller's to choose. Binding it here, in the forwarder, is the
// only place that holds both the request and the identity it belongs to: the
// buildin/mcp task cannot see the caller's token, and mcpScopeCheck is a pure
// yes/no.
//
// Only a scoped token reaches this. An unscoped operator key still drives dev
// mode with whatever run_id it likes, exactly as it can through
// PATCH /api/sources/{name}/dev.
//
// Anything that doesn't parse as a switch_dev_mode envelope is returned
// untouched, ok true — mcpScopeCheck has already decided such a body is
// allowed, and the task's own error path gives it a better message than a
// rewrite could.
//
// ok is false only when a call that does need binding could not be re-encoded.
// The caller must refuse such a request: forwarding the original would hand
// the task the caller's own run_id, which is the one outcome this function
// exists to prevent.
func mcpScopeRewrite(scope *pkgruntime.MCPScope, body []byte) (out []byte, ok bool) {
	raw, decoded := decodeRPCEnvelope(body)
	if !decoded {
		return body, true
	}
	if method, _ := raw["method"].(string); method != "tools/call" {
		return body, true
	}
	params, _ := raw["params"].(map[string]any)
	if name, _ := params["name"].(string); name != "switch_dev_mode" {
		return body, true
	}
	arguments, isObject := params["arguments"].(map[string]any)
	if !isObject {
		// arguments absent or not an object: the call is missing its
		// required `source` either way, so let the task reject it.
		return body, true
	}
	arguments["run_id"] = scope.RunID
	rewritten, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	return rewritten, true
}

// apiQueryRuns handles GET /api/runs with one of three filter modes:
//   - ?parent=<runID>          → list immediate child runs of <runID> (#116)
//   - ?group=<label>&task=<id> → list same-group siblings for a task (#116)
//   - ?root=<runID>            → list <runID>'s whole descendant tree —
//     chain / pipeline stages / suspend-resume continuations — collapsed
//     under their shared root_run_id, oldest first (#569)
//
// Exactly one filter is required; group additionally requires task to scope
// the lookup since labels are task-local.
func (s *Server) apiQueryRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 50
	if v := q.Get("limit"); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}
	parent := q.Get("parent")
	group := q.Get("group")
	root := q.Get("root")
	taskID := q.Get("task")
	if parent == "" && group == "" && root == "" {
		jsonErr(w, "missing filter: provide ?parent=<id>, ?root=<id>, or ?group=<label>&task=<id>", http.StatusBadRequest)
		return
	}
	if group != "" && taskID == "" {
		jsonErr(w, "?group requires ?task=<id>", http.StatusBadRequest)
		return
	}
	var (
		runs []*registry.Run
		err  error
	)
	switch {
	case root != "":
		runs, err = s.registry.ListRunGroup(r.Context(), root, limit)
	case parent != "":
		runs, err = s.registry.ListChildren(r.Context(), parent, limit)
	default:
		runs, err = s.registry.ListByGroup(r.Context(), taskID, group, limit)
	}
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, runs)
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

// RunDetail is the shape returned by GET /api/runs/{runID}. ResumeSchema
// carries the suspended run's JSON Schema as raw JSON so the WebUI renders a
// form from it directly; the embedded Run's own ResumeToken/ResumeState/
// ResumeSchema are cleared by apiGetRun before embedding — the token is the
// resume authorization and must never reach the client, and the state blob is
// task-internal.
type RunDetail struct {
	*registry.Run
	TaskName     string          `json:"task_name"`
	ResumeSchema json.RawMessage `json:"resume_schema,omitempty"`
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
	// Surface the schema (for rendering) but strip the token and state blob from
	// the wire — the resume endpoint resolves the token server-side.
	var schema json.RawMessage
	if run.Status == registry.StatusSuspended && len(run.ResumeSchema) > 0 {
		schema = json.RawMessage(run.ResumeSchema)
	}
	safe := *run
	safe.ResumeToken = ""
	safe.ResumeState = nil
	safe.ResumeSchema = nil
	jsonOK(w, RunDetail{Run: &safe, TaskName: taskName, ResumeSchema: schema})
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

// GroupLogEntryJSON is the JSON shape returned by GET
// /api/runs/{runID}/group-logs — LogEntryJSON plus RunID, since entries in an
// aggregated view span multiple runs and the caller needs to attribute each
// line to its run (#569).
type GroupLogEntryJSON struct {
	ID      int64  `json:"id"`
	RunID   string `json:"run_id"`
	Level   string `json:"level"`
	Message string `json:"message"`
	Ts      int64  `json:"ts"` // Unix milliseconds
}

// apiGetGroupLogs handles GET /api/runs/{runID}/group-logs: the union of log
// entries for every run sharing {runID}'s root_run_id (its whole descendant
// tree), interleaved chronologically. {runID} need not itself be the root —
// GetRunGroupLogs resolves its group membership via COALESCE(root_run_id, id).
func (s *Server) apiGetGroupLogs(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")
	run, err := s.registry.GetRun(r.Context(), runID)
	if err != nil {
		jsonErr(w, "run not found", http.StatusNotFound)
		return
	}
	logs, err := s.registry.GetRunGroupLogs(r.Context(), run.RootRunID)
	if err != nil {
		s.log.Error("get run group logs", zap.String("run", runID), zap.Error(err))
		jsonErr(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]GroupLogEntryJSON, len(logs))
	for i, l := range logs {
		out[i] = GroupLogEntryJSON{
			ID:      l.ID,
			RunID:   l.RunID,
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

// replayRequest is the optional JSON body for POST /api/runs/{runID}/replay.
type replayRequest struct {
	TaskName string `json:"task_name"`
}

// apiReplayRun fires a new run from the persisted input of an existing run.
// The new run's parent_run_id is set to {runID}; triggerSource = "replay"
// (engine guard skips on_failure_chain on its failure).
//
// Status codes:
//   - 200 — replay fired; body returns the new run_id.
//   - 400 — malformed body OR run has no persisted input.
//   - 404 — run not found OR task_name override task not registered.
//   - 500 — internal error (decrypt failure, fire failure).
//   - 503 — replay not available (input persistence disabled / not wired).
func (s *Server) apiReplayRun(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runID")

	var req replayRequest
	if r.Body != nil && r.ContentLength != 0 {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil && err != io.EOF {
			jsonErr(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	if s.replayer == nil {
		jsonErr(w, "replay not available (input persistence disabled)", http.StatusServiceUnavailable)
		return
	}

	// Hold a drain slot across Replay: its InputStore.Fetch delegates to a storage
	// task via fireSync, a top-level run with no enclosing tracked slot. The slot
	// makes Engine.Start's shutdown wait block until that fetch finishes its DB
	// writes, and refuses the replay once shutdown has latched (#533).
	release, ok := s.engine.DrainSlot()
	if !ok {
		jsonErr(w, "engine is shutting down", http.StatusServiceUnavailable)
		return
	}
	defer release()

	newRunID, err := s.replayer.Replay(r.Context(), runID, req.TaskName, "", "")
	if err != nil {
		var taskNotFound *trigger.TaskNotFoundError
		switch {
		case errors.Is(err, registry.ErrRunNotReplayable):
			jsonErr(w, "run is suspended; resume it instead of replaying: "+runID, http.StatusConflict)
		case errors.Is(err, registry.ErrInputUnavailable):
			jsonErr(w, "no persisted input for run: "+runID, http.StatusBadRequest)
		case errors.Is(err, registry.ErrRunNotFound):
			jsonErr(w, "run not found: "+runID, http.StatusNotFound)
		case errors.As(err, &taskNotFound):
			jsonErr(w, err.Error(), http.StatusNotFound)
		default:
			jsonErr(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	jsonOK(w, map[string]any{"run_id": newRunID})
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
	err := s.updateConfig(func(cfg *config.Config) error {
		cfg.AI.Task = task
		return nil
	})
	if err != nil {
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
	err := s.updateConfig(func(cfg *config.Config) error {
		if body.LogLevel != "" {
			cfg.LogLevel = body.LogLevel
		}
		if body.Secret != "" {
			cfg.Server.Secret = body.Secret
		}
		return nil
	})
	if err != nil {
		s.log.Warn("settings persist failed", zap.Error(err))
		jsonErr(w, "saved in memory but could not write file: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.log.Info("server settings updated")
	jsonOK(w, map[string]string{"status": "ok"})
}

// errSourceExists aborts the apiAddSource updateConfig mutate so a duplicate
// name never triggers a config persist — see the TOCTOU comment inline in
// apiAddSource for why the check and the map write must share one mutate call.
var errSourceExists = errors.New("source exists")

func (s *Server) apiAddSource(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string `json:"name"`
		Path     string `json:"path"`
		URL      string `json:"url"`
		Branch   string `json:"branch"`
		Tag      string `json:"tag"`
		TokenEnv string `json:"token_env"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, "bad request", http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(body.Name)
	if name == "" {
		jsonErr(w, "name is required", http.StatusBadRequest)
		return
	}
	if err := validateSourceName(name); err != nil {
		jsonErr(w, err.Error(), http.StatusBadRequest)
		return
	}

	var ref taskset.Ref
	if url := strings.TrimSpace(body.URL); url != "" {
		if !isAllowedGitScheme(url) {
			jsonErr(w, "url scheme not allowed", http.StatusBadRequest)
			return
		}
		branch, tag := strings.TrimSpace(body.Branch), strings.TrimSpace(body.Tag)
		// Mirrors applyDefaults: a pinned ref must not also carry a branch.
		if branch == "" && tag == "" {
			branch = taskset.DefaultBranch
		}
		ref = taskset.Ref{
			URL:          url,
			Branch:       branch,
			Tag:          tag,
			PollInterval: 30 * 1e9,
			Auth:         taskset.RefAuth{TokenEnv: body.TokenEnv},
		}
		// Same rules config-load applies: this handler must not write an entry
		// that stops the daemon booting on its next start.
		if err := taskset.ValidateRefTarget(&ref); err != nil {
			jsonErr(w, err.Error(), http.StatusBadRequest)
			return
		}
	} else if path := strings.TrimSpace(body.Path); path != "" {
		watchTrue := true
		ref = taskset.Ref{Path: path, Watch: &watchTrue}
	} else {
		jsonErr(w, "either url or path is required", http.StatusBadRequest)
		return
	}

	entry := &taskset.Entry{Ref: &ref}

	// Name-qualified so two different source names that happen to reference
	// the identical git URL or local path (e.g. a source added here pointing
	// at a path an existing source already watches, issue #621) never
	// collide on Source.ID() — see taskset.SourceID's doc comment for why
	// that collision matters (pkg/registry/reconciler.go's rc.cancels is
	// keyed by this exact value).
	id := taskset.SourceID(name, &ref)

	// Atomically check-and-claim the name against the live config's
	// spec.entries — the same map that SourceManager.List reads from, so
	// this is the single source of truth for "does this name already
	// exist" rather than re-reading dicode.yaml from disk. The existence
	// check and the map write both happen inside updateConfig's mutate
	// callback, i.e. under a single s.cfgMu.Lock critical section, so two
	// concurrent requests for the same brand-new name can't both observe
	// "not present" before either writes back (TOCTOU). errSourceExists
	// aborts the mutate before persistConfigLocked runs, so a rejected
	// duplicate never touches disk or the in-memory map.
	persistErr := s.updateConfig(func(cfg *config.Config) error {
		if cfg.Spec.Entries == nil {
			cfg.Spec.Entries = make(map[string]*taskset.Entry)
		}
		if _, exists := cfg.Spec.Entries[name]; exists {
			return errSourceExists
		}
		cfg.Spec.Entries[name] = entry
		return nil
	})
	if errors.Is(persistErr, errSourceExists) {
		jsonErr(w, "source \""+name+"\" already exists", http.StatusConflict)
		return
	}
	if persistErr != nil {
		s.log.Warn("source persist failed", zap.Error(persistErr))
	}

	// Match the daemon's buildTaskSetSourceFromEntry: forward entry.Overrides
	// so the source applies any future overrides patched in via the REST API.
	// entry.Overrides is always nil for a freshly constructed entry today;
	// the guard is defensive in case a future code path (e.g. clone-with-
	// overrides) populates it before this call.
	s.cfgMu.RLock()
	allowedTokenEnvs := s.cfg.SourceSecurity.AllowedTokenEnvs
	s.cfgMu.RUnlock()
	opts := []taskset.SourceOption{taskset.WithAllowedTokenEnvs(allowedTokenEnvs)}
	if entry.Overrides != nil {
		opts = append(opts, taskset.WithParentOverrides(entry.Overrides))
	}
	// The claim above is deliberately done before this potentially slow,
	// filesystem/network-touching registration step so the two are not
	// serialised behind s.cfgMu — only the fast map check-and-write is.
	ts := taskset.NewSource(id, name, &ref, "", s.dataDir, false, ref.PollInterval, s.log, opts...)
	if s.reconciler != nil {
		if err := s.reconciler.AddSource(ts); err != nil {
			// Registration failed after the claim succeeded: roll back so a
			// source that never actually started doesn't linger as a
			// phantom entry in cfg.Spec.Entries (and on disk).
			//
			// Only delete if cfg.Spec.Entries[name] is still THIS request's
			// entry pointer. Comparing by name presence alone is an ABA
			// hazard: if this rollback runs late (e.g. after a concurrent
			// DELETE + re-POST for the same name), cfg.Spec.Entries[name]
			// could already hold a newer, unrelated entry that has since won
			// the name — deleting it would silently destroy that valid claim.
			if rollbackErr := s.updateConfig(func(cfg *config.Config) error {
				if cfg.Spec.Entries[name] == entry {
					delete(cfg.Spec.Entries, name)
				}
				return nil
			}); rollbackErr != nil {
				s.log.Warn("source claim rollback failed", zap.String("name", name), zap.Error(rollbackErr))
			}
			jsonErr(w, "start source: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	// reconcileClaimAfterAdd guards against a race with apiRemoveSource (and,
	// via the entry pointer, a subsequent concurrent re-add): see its doc
	// comment for the full mechanics. Only register ts with sourceMgr if our
	// cfg.Spec.Entries[name] claim — this exact *entry — is still standing.
	if s.reconcileClaimAfterAdd(name, id, entry) && s.sourceMgr != nil {
		s.sourceMgr.Register(name, ts)
	}

	s.log.Info("source added", zap.String("name", name))
	jsonOK(w, map[string]string{"status": "ok"})
}

// reconcileClaimAfterAdd re-validates, immediately after s.reconciler.AddSource
// for (name, id) has returned successfully, that the cfg.Spec.Entries[name]
// claim made earlier by apiAddSource's updateConfig callback — specifically,
// THIS request's entry pointer — is still the one occupying that slot. Call
// it right before registering the newly started source with sourceMgr.
//
// Why the re-check is needed: reconciler.AddSource's underlying startSource
// calls src.Start synchronously *before* it populates rc.cancels[id] (see
// pkg/registry/reconciler.go's startSource). If a concurrent apiRemoveSource
// ran while that Start was in flight, it would have seen our claimed
// cfg.Spec.Entries[name], deleted it, and called reconciler.RemoveSource(id)
// — but that call would have missed (rc.cancels[id] not populated yet) and
// been a silent no-op, leaving AddSource free to finish "successfully" a
// moment later. Without this re-check apiAddSource would unconditionally
// register a live source in sourceMgr with no corresponding config entry:
// the orphan the 3fafc3f rollback was built to prevent, from the other
// direction.
//
// Why identity, not just name presence: name presence alone is an ABA
// hazard. If, after the concurrent delete above, a THIRD request re-adds the
// same name before this call runs, cfg.Spec.Entries[name] is present again
// but holds a different, newer *taskset.Entry. Treating "present" as "still
// ours" would register this request's now-stale ts into sourceMgr on top of
// (or racing with) the newer request's own registration, and would leak this
// source's reconciler-side registration forever (nothing else will ever call
// RemoveSource(id) for it). Comparing against the specific entry pointer this
// request claimed distinguishes "our claim still stands" from "the slot is
// now occupied by someone else's newer claim" — both cases must self-clean
// via reconciler.RemoveSource(id) and must NOT touch the entry actually
// occupying the slot (whether that's nothing, or a newer valid claim).
//
// Re-checking here — now that AddSource has returned and rc.cancels[id] is
// guaranteed populated — lets us detect a lost race and self-clean via the
// same teardown apiRemoveSource itself uses (reconciler.RemoveSource),
// instead of registering an orphan. We must NOT resurrect or otherwise
// mutate cfg.Spec.Entries[name]: whatever is (or isn't) there already
// reflects the outcome of whichever concurrent request won.
//
// Returns true if entry is still the one occupying cfg.Spec.Entries[name]
// and the caller should proceed to register the source with sourceMgr; false
// if a concurrent removal — or removal-then-re-add — won the race, in which
// case this function has already torn the source back down via
// reconciler.RemoveSource(id) and the caller must not register it.
// A nil s.reconciler always returns true (nothing to race against).
func (s *Server) reconcileClaimAfterAdd(name, id string, entry *taskset.Entry) bool {
	if s.reconciler == nil {
		return true
	}
	s.cfgMu.RLock()
	current := s.cfg.Spec.Entries[name]
	s.cfgMu.RUnlock()
	if current == entry {
		return true
	}
	s.reconciler.RemoveSource(id)
	s.log.Info("source removed (or replaced by a newer claim) concurrently while being added; cleaned up orphaned source",
		zap.String("name", name), zap.String("id", id))
	return false
}

// apiListGitBranches lists remote branches for a git URL — used by the config
// UI when adding or inspecting a source.
//
// Security (#475): the token_env query parameter names an environment
// variable whose value is sent as an HTTP basic-auth password to the remote.
// Honouring an arbitrary name would let any authenticated caller exfiltrate
// any daemon env var by pointing url at a server they control. The rule
// enforced by resolveBranchesTokenEnv:
//
//  1. If url matches an already-configured git source, that source's
//     configured auth.token_env is used and the request's token_env is
//     ignored.
//  2. Otherwise (previewing a source that has not been added yet), token_env
//     must be empty or exactly match an auth.token_env already configured on
//     some git source in dicode.yaml — i.e. an env var the operator has
//     explicitly designated as a git credential.
//
// SSRF against loopback/private/link-local/internal hosts is rejected inside
// gitSource.ListBranches before any connection is made.
func (s *Server) apiListGitBranches(w http.ResponseWriter, r *http.Request) {
	repoURL := r.URL.Query().Get("url")
	if repoURL == "" {
		jsonErr(w, "url is required", http.StatusBadRequest)
		return
	}
	if !isAllowedGitScheme(repoURL) {
		jsonErr(w, "url scheme not allowed", http.StatusBadRequest)
		return
	}
	tokenEnv, err := s.resolveBranchesTokenEnv(repoURL, r.URL.Query().Get("token_env"))
	if err != nil {
		jsonErr(w, err.Error(), http.StatusForbidden)
		return
	}
	branches, err := gitSource.ListBranches(r.Context(), repoURL, tokenEnv)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonOK(w, branches)
}

// resolveBranchesTokenEnv decides which environment variable, if any, may be
// used as the git credential for a branch-listing request. See the security
// note on apiListGitBranches for the rule it enforces.
func (s *Server) resolveBranchesTokenEnv(repoURL, requested string) (string, error) {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()

	want := normalizeGitURL(repoURL)
	allowed := make(map[string]bool)
	if s.cfg != nil {
		for _, entry := range s.cfg.Spec.Entries {
			if entry == nil || entry.Ref == nil || !entry.Ref.IsGit() {
				continue
			}
			if normalizeGitURL(entry.Ref.URL) == want {
				// Configured source: always use its own credential.
				return entry.Ref.Auth.TokenEnv, nil
			}
			if entry.Ref.Auth.TokenEnv != "" {
				allowed[entry.Ref.Auth.TokenEnv] = true
			}
		}
	}
	if requested == "" || allowed[requested] {
		return requested, nil
	}
	return "", fmt.Errorf("token_env %q is not permitted: it must match the auth.token_env of a git source configured in dicode.yaml", requested)
}

// normalizeGitURL canonicalises a git remote URL for equality comparison:
// trims surrounding whitespace, a trailing slash, and a trailing ".git".
func normalizeGitURL(raw string) string {
	u := strings.TrimSpace(raw)
	u = strings.TrimSuffix(u, "/")
	u = strings.TrimSuffix(u, ".git")
	return u
}

// errSourceNotFound aborts the apiRemoveSource updateConfig mutate so a
// missing source never triggers a config persist. The handler surfaces it
// as the 404 via its own entry-nil check.
var errSourceNotFound = errors.New("source not found")

func (s *Server) apiRemoveSource(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		jsonErr(w, "source name is required", http.StatusBadRequest)
		return
	}
	var entry *taskset.Entry
	persistErr := s.updateConfig(func(cfg *config.Config) error {
		entry = cfg.Spec.Entries[name]
		if entry == nil {
			return errSourceNotFound // skip the persist; surfaced as a 404 below
		}
		delete(cfg.Spec.Entries, name)
		return nil
	})
	if entry == nil {
		jsonErr(w, "source not found", http.StatusNotFound)
		return
	}

	if s.reconciler != nil && entry.Ref != nil {
		// Must match the id apiAddSource claimed via taskset.SourceID — see
		// its call site's comment for why the name component is required.
		id := taskset.SourceID(name, entry.Ref)
		s.reconciler.RemoveSource(id)
	}
	if persistErr != nil {
		s.log.Warn("source persist failed", zap.Error(persistErr))
	}
	s.log.Info("source removed", zap.String("name", name))
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
	s.cfgMu.RLock()
	pinnedVersions := make(map[string]string, len(s.cfg.Runtimes))
	for name, rc := range s.cfg.Runtimes {
		pinnedVersions[name] = rc.Version
	}
	s.cfgMu.RUnlock()

	var out []RuntimeInfo
	for _, mgr := range s.managedRuntimes {
		version := pinnedVersions[mgr.Name()]
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

// runtimeVersionPattern bounds a runtime version to the shape Deno and uv
// actually publish. The value is interpolated into both a cache path under
// ~/.cache/dicode and a release download URL, so anything outside this shape —
// a separator, a "..", a scheme — is a traversal or URL-redirection attempt
// rather than a version.
var runtimeVersionPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+){0,2}(-[0-9A-Za-z.]+)?$`)

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
		s.cfgMu.RLock()
		rc, ok := s.cfg.Runtimes[name]
		s.cfgMu.RUnlock()
		if ok && rc.Version != "" {
			version = rc.Version
		} else {
			version = mgr.DefaultVersion()
		}
	}
	if !runtimeVersionPattern.MatchString(version) {
		jsonErr(w, "invalid version", http.StatusBadRequest)
		return
	}

	s.log.Info("installing runtime", zap.String("runtime", name), zap.String("version", version))
	if err := mgr.Install(r.Context(), version); err != nil {
		s.log.Error("runtime install failed", zap.String("runtime", name), zap.Error(err))
		jsonErr(w, "install failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	persistErr := s.updateConfig(func(cfg *config.Config) error {
		if cfg.Runtimes == nil {
			cfg.Runtimes = make(map[string]config.RuntimeConfig)
		}
		rcUpdate := cfg.Runtimes[name]
		rcUpdate.Version = version
		cfg.Runtimes[name] = rcUpdate
		return nil
	})
	if persistErr != nil {
		s.log.Warn("runtime config persist failed", zap.Error(persistErr))
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
	persistErr := s.updateConfig(func(cfg *config.Config) error {
		if cfg.Runtimes != nil {
			delete(cfg.Runtimes, name)
		}
		return nil
	})
	if persistErr != nil {
		s.log.Warn("runtime config persist failed", zap.Error(persistErr))
	}
	jsonOK(w, map[string]string{"status": "ok"})
}

// updateConfig runs mutate on the in-memory config and persists the result
// to dicode.yaml under a single cfgMu critical section — the same
// lock-across-mutate+persist scope every call site previously held inline.
// A non-nil error from mutate skips the persist and is returned as-is;
// otherwise the persistConfigLocked error is returned for the caller to
// surface (the sites differ: hard 500 vs warn-and-continue).
func (s *Server) updateConfig(mutate func(*config.Config) error) error {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if err := mutate(s.cfg); err != nil {
		return err
	}
	return s.persistConfigLocked()
}

// persistConfig writes the current in-memory config back to dicode.yaml.
// persistConfigLocked serialises the in-memory cfg back to dicode.yaml.
// CALLER MUST HOLD s.cfgMu.Lock — this function reads every cfg field
// without re-acquiring the mutex.
func (s *Server) persistConfigLocked() error {
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

	// Persist spec.entries as the new `spec:` block in the YAML document.
	// Entries are marshalled via yaml.Marshal so that all fields (ref, overrides,
	// tags, inline) round-trip without the serialiser needing to know about every
	// field individually. Keys are sorted for deterministic output — non-sorted
	// map iteration produces noisy git diffs on every save.
	if len(s.cfg.Spec.Entries) > 0 {
		entryKeys := make([]string, 0, len(s.cfg.Spec.Entries))
		for k := range s.cfg.Spec.Entries {
			entryKeys = append(entryKeys, k)
		}
		sort.Strings(entryKeys)

		specEntries := make(map[string]any, len(s.cfg.Spec.Entries))
		for _, name := range entryKeys {
			entry := s.cfg.Spec.Entries[name]
			if entry == nil {
				continue
			}
			// Round-trip via yaml.Marshal → yaml.Unmarshal into map[string]any so
			// that all Entry fields (ref, overrides, tags, inline) are preserved
			// without enumerating them here.
			entryYAML, err := yaml.Marshal(entry)
			if err != nil {
				return fmt.Errorf("marshal entry %q: %w", name, err)
			}
			var entryMap map[string]any
			if err := yaml.Unmarshal(entryYAML, &entryMap); err != nil {
				return fmt.Errorf("unmarshal entry %q: %w", name, err)
			}
			specEntries[name] = entryMap
		}
		specBlock, _ := doc["spec"].(map[string]any)
		if specBlock == nil {
			specBlock = map[string]any{}
		}
		specBlock["entries"] = specEntries
		doc["spec"] = specBlock
	} else {
		if specBlock, ok := doc["spec"].(map[string]any); ok {
			delete(specBlock, "entries")
			if len(specBlock) == 0 {
				delete(doc, "spec")
			}
		}
	}

	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	return config.AtomicWriteFile(s.cfgPath, out, 0600)
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

// pipelineTriggerLabel renders a human-readable trigger summary for a
// kind: PipelineTask. A pipeline's PipelineTrigger has no Daemon shape — a
// pipeline is daemon-shaped iff its terminal stage is a daemon Task, which
// the detail view surfaces separately via DaemonState.
func pipelineTriggerLabel(p *task.PipelineTask) string {
	tc := p.Trigger
	switch {
	case tc.Cron != "":
		return "cron: " + tc.Cron
	case tc.Webhook != "":
		return "webhook: " + tc.Webhook
	case tc.Chain != nil:
		return "chain: " + tc.Chain.From
	case tc.Manual:
		return "manual"
	default:
		return "—"
	}
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
