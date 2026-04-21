package ipc

import (
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Gateway is the daemon's HTTP dispatch layer. It maintains a priority-ordered
// table of pattern→handler registrations and routes incoming requests to the
// longest matching entry.
//
// Two kinds of handlers are supported:
//
//  1. Go handlers — registered directly via Register; used for webhook tasks
//     (auto-registered by the reconciler) and built-in routes.
//
//  2. IPC handlers — registered by daemon tasks via the http.register IPC
//     method. Incoming HTTP requests are forwarded to the task over its open
//     IPC connection as HTTPInboundRequest pushes; the task replies with an
//     http.respond message.
//
// Priority: exact match > longest-prefix match > 404.
type Gateway struct {
	mu     sync.RWMutex
	routes []gatewayRoute // sorted longest-prefix first
}

type gatewayRoute struct {
	pattern string
	handler http.Handler
}

// NewGateway creates an empty Gateway.
func NewGateway() *Gateway { return &Gateway{} }

// Register adds or replaces a Go http.Handler for the given URL pattern.
// Pattern matching uses prefix semantics — a pattern of "/hooks/push" matches
// "/hooks/push", "/hooks/push/", "/hooks/push/assets/logo.png", etc.
// An exact-length pattern wins over a shorter prefix when both match.
func (g *Gateway) Register(pattern string, h http.Handler) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, r := range g.routes {
		if r.pattern == pattern {
			g.routes[i].handler = h
			return
		}
	}
	g.routes = append(g.routes, gatewayRoute{pattern: pattern, handler: h})
	sort.Slice(g.routes, func(i, j int) bool {
		return len(g.routes[i].pattern) > len(g.routes[j].pattern) // longest first
	})
}

// Unregister removes the handler for pattern. No-op if not registered.
func (g *Gateway) Unregister(pattern string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, r := range g.routes {
		if r.pattern == pattern {
			g.routes = append(g.routes[:i], g.routes[i+1:]...)
			return
		}
	}
}

// ServeHTTP dispatches r to the longest matching registered pattern.
// Returns 404 if nothing matches.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.RLock()
	var matched http.Handler
	for _, route := range g.routes {
		if PathMatches(route.pattern, r.URL.Path) {
			matched = route.handler
			break // routes are sorted longest-first; first match wins
		}
	}
	g.mu.RUnlock()

	if matched != nil {
		matched.ServeHTTP(w, r)
		return
	}
	http.NotFound(w, r)
}

// PathMatches reports whether urlPath is matched by pattern.
// A pattern matches if urlPath equals it, or starts with pattern+"/" — with
// any trailing slash on the pattern normalised away first so "/hooks/foo/"
// and "/hooks/foo" both match "/hooks/foo/bar". Exported so webhook auth
// gates outside this package can share the same rule; drift between the
// gateway (dispatches) and an auth guard (decides whether to dispatch) has
// been the source of one security bug already.
func PathMatches(pattern, urlPath string) bool {
	if urlPath == pattern {
		return true
	}
	return strings.HasPrefix(urlPath, strings.TrimSuffix(pattern, "/")+"/")
}

// ── IPC handler (daemon tasks that call http.register) ────────────────────────

// ipcHandler is an http.Handler that forwards requests to a daemon task over
// its open IPC connection. The gateway pushes an HTTPInboundRequest; the task
// replies with http.respond. A 60-second timeout applies per request.
type ipcHandler struct {
	writeMu sync.Mutex          // serialises concurrent writes to push
	push    func(msg any) error // writes to the task's IPC connection
	pending sync.Map            // requestID → chan ipcResponse
}

type ipcResponse struct {
	status  int
	headers map[string]string
	body    []byte
}

func (h *ipcHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rid := uuid.New().String()
	ch := make(chan ipcResponse, 1)
	h.pending.Store(rid, ch)
	defer h.pending.Delete(rid)

	// Collect request body (up to 8 MiB — matches maxMessageSize).
	// io.LimitReader caps the read and io.ReadAll surfaces any real I/O error.
	var body []byte
	if r.Body != nil {
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, maxMessageSize))
		if err != nil {
			http.Error(w, "gateway: read request body failed", http.StatusBadRequest)
			return
		}
	}

	// Collect relevant request headers.
	headers := make(map[string]string, len(r.Header))
	for k := range r.Header {
		headers[k] = r.Header.Get(k)
	}

	inbound := HTTPInboundRequest{
		RequestID:  rid,
		HTTPMethod: r.Method,
		Path:       r.URL.Path,
		Query:      r.URL.RawQuery,
		ReqHeaders: headers,
		ReqBody:    body,
	}
	h.writeMu.Lock()
	err := h.push(inbound)
	h.writeMu.Unlock()
	if err != nil {
		http.Error(w, "gateway: send to handler failed", http.StatusBadGateway)
		return
	}

	t := time.NewTimer(60 * time.Second)
	defer t.Stop()
	select {
	case resp := <-ch:
		for k, v := range resp.headers {
			w.Header().Set(k, v)
		}
		if resp.status != 0 {
			w.WriteHeader(resp.status)
		}
		_, _ = w.Write(resp.body)
	case <-t.C:
		http.Error(w, "gateway: handler timeout", http.StatusGatewayTimeout)
	case <-r.Context().Done():
		// Client disconnected; nothing to write.
	}
}

// complete delivers a task's http.respond reply to the waiting ServeHTTP call.
// Returns false if rid has no pending request (already timed out or unknown).
func (h *ipcHandler) complete(rid string, status int, headers map[string]string, body []byte) bool {
	v, ok := h.pending.Load(rid)
	if !ok {
		return false
	}
	v.(chan ipcResponse) <- ipcResponse{status: status, headers: headers, body: body}
	return true
}
