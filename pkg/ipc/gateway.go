package ipc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// ReservedOAuthCompletePath is the webhook path the relay broker delivers
// encrypted OAuth tokens to. Only OAuthRelayBuiltinID may claim it: any other
// owner would be an exfiltration sink for decrypted credentials.
const (
	ReservedOAuthCompletePath = "/hooks/oauth-complete"
	OAuthRelayBuiltinID       = "buildin/auth-relay"
)

// RouteKind is what a gateway route was claimed for.
type RouteKind int

const (
	RouteTaskWebhook     RouteKind = iota + 1 // kind: Task trigger.webhook
	RoutePipelineWebhook                      // kind: PipelineTask trigger.webhook
	RouteHTTPRegister                         // a daemon task's http.register
)

func (k RouteKind) String() string {
	switch k {
	case RouteTaskWebhook:
		return "task-webhook"
	case RoutePipelineWebhook:
		return "pipeline-webhook"
	case RouteHTTPRegister:
		return "http.register"
	default:
		return fmt.Sprintf("RouteKind(%d)", int(k))
	}
}

var (
	// ErrRouteClaimed is returned by Claim when another task owns the pattern.
	ErrRouteClaimed = errors.New("route already claimed by another task")
	// ErrRouteReserved is returned by Claim for ReservedOAuthCompletePath.
	ErrRouteReserved = errors.New("route is reserved")
	// ErrRouteInvalid is returned by Claim for a pattern that is not an
	// absolute path below "/".
	ErrRouteInvalid = errors.New("route pattern must be an absolute path below /")
)

// Gateway is the daemon's single HTTP route table. Every webhook trigger and
// every daemon-task http.register route is a claim here, recording its owning
// task and kind; the gateway's dispatch, the engine's webhook handler and the
// webui auth guard all read the same Resolve answer.
//
// Matching: an exact pattern match, else the longest pattern p such that the
// path starts with p+"/" (the remainder is the asset path). Patterns are
// normalised once at Claim by trimming a trailing slash.
type Gateway struct {
	mu     sync.RWMutex
	routes []gatewayRoute // sorted longest pattern first
}

type gatewayRoute struct {
	pattern string
	owner   string
	kind    RouteKind
	handler http.Handler
}

// RouteMatch is the route a request path resolves to.
type RouteMatch struct {
	TaskID    string
	Kind      RouteKind
	Pattern   string // the normalised claimed pattern
	AssetPath string // the path below Pattern; "" for the pattern itself
	handler   http.Handler
}

// NewGateway creates an empty Gateway.
func NewGateway() *Gateway { return &Gateway{} }

// NormalizePattern is the form Claim stores a pattern in.
func NormalizePattern(pattern string) string { return strings.TrimSuffix(pattern, "/") }

// Claim binds pattern to owner's handler. A re-claim by the same owner
// replaces the handler and kind; a claim of a pattern another owner holds
// fails with ErrRouteClaimed and leaves the existing route serving.
func (g *Gateway) Claim(owner, pattern string, kind RouteKind, h http.Handler) error {
	p := NormalizePattern(pattern)
	if p == "" || p[0] != '/' {
		return fmt.Errorf("%w: %q", ErrRouteInvalid, pattern)
	}
	if p == ReservedOAuthCompletePath && owner != OAuthRelayBuiltinID {
		return fmt.Errorf("%w: %s may only be claimed by %s", ErrRouteReserved, p, OAuthRelayBuiltinID)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, r := range g.routes {
		if r.pattern != p {
			continue
		}
		if r.owner != owner {
			return fmt.Errorf("%w: %s is held by %s", ErrRouteClaimed, p, r.owner)
		}
		g.routes[i].kind = kind
		g.routes[i].handler = h
		return nil
	}
	g.routes = append(g.routes, gatewayRoute{pattern: p, owner: owner, kind: kind, handler: h})
	sort.SliceStable(g.routes, func(i, j int) bool {
		return len(g.routes[i].pattern) > len(g.routes[j].pattern)
	})
	return nil
}

// Release drops every route owner holds except the pattern keep (normalised
// like Claim; "" keeps nothing).
func (g *Gateway) Release(owner, keep string) {
	keep = NormalizePattern(keep)
	g.mu.Lock()
	defer g.mu.Unlock()
	kept := g.routes[:0]
	for _, r := range g.routes {
		if r.owner == owner && (keep == "" || r.pattern != keep) {
			continue
		}
		kept = append(kept, r)
	}
	clear(g.routes[len(kept):])
	g.routes = kept
}

// releaseHandler drops pattern only while it is still served by h, so a
// closing IPC connection cannot remove a route a newer run of the same task
// has re-claimed.
func (g *Gateway) releaseHandler(pattern string, h *ipcHandler) {
	p := NormalizePattern(pattern)
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, r := range g.routes {
		if r.pattern != p {
			continue
		}
		if cur, ok := r.handler.(*ipcHandler); ok && cur == h {
			g.routes = append(g.routes[:i], g.routes[i+1:]...)
		}
		return
	}
}

// Resolve returns the route urlPath dispatches to.
func (g *Gateway) Resolve(urlPath string) (RouteMatch, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	for _, r := range g.routes {
		if urlPath == r.pattern {
			return RouteMatch{TaskID: r.owner, Kind: r.kind, Pattern: r.pattern, handler: r.handler}, true
		}
		if strings.HasPrefix(urlPath, r.pattern+"/") {
			return RouteMatch{TaskID: r.owner, Kind: r.kind, Pattern: r.pattern, AssetPath: urlPath[len(r.pattern)+1:], handler: r.handler}, true
		}
	}
	return RouteMatch{}, false
}

type routeCtxKey struct{}

// WithRoute pins the route a request dispatches to, so a caller that already
// resolved it (the auth guard) and the gateway act on the same answer.
func WithRoute(ctx context.Context, m RouteMatch) context.Context {
	return context.WithValue(ctx, routeCtxKey{}, m)
}

// RouteFromContext returns the route pinned by WithRoute or by the gateway's
// dispatch.
func RouteFromContext(ctx context.Context) (RouteMatch, bool) {
	m, ok := ctx.Value(routeCtxKey{}).(RouteMatch)
	return m, ok
}

// ServeHTTP dispatches r to its route (pinned in the context, else resolved),
// passing the match on in the request context. 404 if nothing matches.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m, ok := RouteFromContext(r.Context())
	if !ok {
		if m, ok = g.Resolve(r.URL.Path); ok {
			r = r.WithContext(WithRoute(r.Context(), m))
		}
	}
	if !ok || m.handler == nil {
		http.NotFound(w, r)
		return
	}
	m.handler.ServeHTTP(w, r)
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
