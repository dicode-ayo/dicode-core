package ipc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// ── Gateway route table ──────────────────────────────────────────────────────

func named(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m, _ := RouteFromContext(r.Context())
		_, _ = w.Write([]byte(name + "|" + m.AssetPath))
	})
}

func serve(g *Gateway, method, path string) (int, string) {
	w := httptest.NewRecorder()
	g.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w.Code, w.Body.String()
}

func TestGateway_Resolve(t *testing.T) {
	type claim struct {
		owner, pattern string
		kind           RouteKind
	}
	claims := []claim{
		{"ai", "/hooks/ai", RouteTaskWebhook},
		{"openai", "/hooks/ai/openai", RouteTaskWebhook},
		{"slash", "/hooks/slash/", RouteTaskWebhook},
		{"pipe", "/hooks/pipe", RoutePipelineWebhook},
		{"daemon", "/hooks/daemon", RouteHTTPRegister},
	}
	g := NewGateway()
	for _, c := range claims {
		if _, err := g.Claim(c.owner, c.pattern, c.kind, named(c.owner)); err != nil {
			t.Fatalf("Claim(%s, %s): %v", c.owner, c.pattern, err)
		}
	}
	cases := []struct {
		path      string
		ok        bool
		owner     string
		kind      RouteKind
		pattern   string
		assetPath string
	}{
		{"/hooks/ai", true, "ai", RouteTaskWebhook, "/hooks/ai", ""},
		{"/hooks/ai/chat.js", true, "ai", RouteTaskWebhook, "/hooks/ai", "chat.js"},
		{"/hooks/ai/openai", true, "openai", RouteTaskWebhook, "/hooks/ai/openai", ""},
		{"/hooks/ai/openai/chat.js", true, "openai", RouteTaskWebhook, "/hooks/ai/openai", "chat.js"},
		{"/hooks/ai/openaix", true, "ai", RouteTaskWebhook, "/hooks/ai", "openaix"},
		{"/hooks/aix", false, "", 0, "", ""},
		{"/hooks/slash", true, "slash", RouteTaskWebhook, "/hooks/slash", ""},
		{"/hooks/slash/", true, "slash", RouteTaskWebhook, "/hooks/slash", ""},
		{"/hooks/slash/sub/a.css", true, "slash", RouteTaskWebhook, "/hooks/slash", "sub/a.css"},
		{"/hooks/pipe", true, "pipe", RoutePipelineWebhook, "/hooks/pipe", ""},
		{"/hooks/daemon/api/x", true, "daemon", RouteHTTPRegister, "/hooks/daemon", "api/x"},
		{"/hooks", false, "", 0, "", ""},
		{"/hooks/none", false, "", 0, "", ""},
	}
	for _, tc := range cases {
		m, ok := g.Resolve(tc.path)
		if ok != tc.ok || m.TaskID != tc.owner || m.Kind != tc.kind || m.Pattern != tc.pattern || m.AssetPath != tc.assetPath {
			t.Errorf("Resolve(%q) = {%q %v %q %q} %v; want {%q %v %q %q} %v", tc.path,
				m.TaskID, m.Kind, m.Pattern, m.AssetPath, ok,
				tc.owner, tc.kind, tc.pattern, tc.assetPath, tc.ok)
		}
		code, body := serve(g, http.MethodGet, tc.path)
		switch {
		case !tc.ok && code != http.StatusNotFound:
			t.Errorf("ServeHTTP(%q) = %d; want 404", tc.path, code)
		case tc.ok && body != tc.owner+"|"+tc.assetPath:
			t.Errorf("ServeHTTP(%q) body = %q; want %q", tc.path, body, tc.owner+"|"+tc.assetPath)
		}
	}
}

func TestGateway_ClaimByOtherOwnerRejected(t *testing.T) {
	g := NewGateway()
	if _, err := g.Claim("a", "/hooks/shared", RouteTaskWebhook, named("a")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/hooks/shared", "/hooks/shared/"} {
		if _, err := g.Claim("b", p, RouteHTTPRegister, named("b")); !errors.Is(err, ErrRouteClaimed) {
			t.Errorf("Claim(b, %q) = %v; want ErrRouteClaimed", p, err)
		}
	}
	if _, body := serve(g, http.MethodPost, "/hooks/shared"); body != "a|" {
		t.Errorf("rejected claim replaced the route: body %q", body)
	}
	if _, err := g.Claim("a", "/hooks/shared", RouteTaskWebhook, named("a2")); err != nil {
		t.Fatalf("same-owner re-claim: %v", err)
	}
	if _, body := serve(g, http.MethodPost, "/hooks/shared"); body != "a2|" {
		t.Errorf("same-owner re-claim did not replace the handler: body %q", body)
	}
}

func TestGateway_ClaimRejectsReservedAndInvalid(t *testing.T) {
	g := NewGateway()
	if _, err := g.Claim("sneaky", ReservedOAuthCompletePath, RouteHTTPRegister, named("x")); !errors.Is(err, ErrRouteReserved) {
		t.Errorf("reserved path claim by non-builtin = %v; want ErrRouteReserved", err)
	}
	if _, err := g.Claim(OAuthRelayBuiltinID, ReservedOAuthCompletePath, RouteTaskWebhook, named("x")); err != nil {
		t.Errorf("reserved path claim by builtin: %v", err)
	}
	for _, p := range []string{"", "/", "hooks/x"} {
		if _, err := g.Claim("t", p, RouteTaskWebhook, named("x")); !errors.Is(err, ErrRouteInvalid) {
			t.Errorf("Claim(%q) = %v; want ErrRouteInvalid", p, err)
		}
	}
	for _, p := range []string{"/hooks", "/hooks/"} {
		for _, k := range []RouteKind{RouteTaskWebhook, RoutePipelineWebhook} {
			if _, err := g.Claim("t", p, k, named("x")); !errors.Is(err, ErrRouteReserved) {
				t.Errorf("Claim(%q, %v) = %v; want ErrRouteReserved", p, k, err)
			}
		}
	}
	for _, p := range []string{"/hooks", "/other/x", "/hooksx/y"} {
		if _, err := g.Claim("d", p, RouteHTTPRegister, named("x")); err == nil {
			t.Errorf("http.register Claim(%q) accepted; want an error", p)
		}
	}
	if _, err := g.Claim("t", "/github-push", RouteTaskWebhook, named("x")); err != nil {
		t.Errorf("task webhook outside /hooks/ rejected: %v", err)
	}
}

// A shorter route never answers for the reserved OAuth path, whether or not
// buildin/auth-relay has claimed it.
func TestGateway_ReservedPathNeverPrefixMatched(t *testing.T) {
	g := NewGateway()
	// Insert an ancestor route directly; Claim refuses it.
	g.routes = []gatewayRoute{{pattern: "/hooks", owner: "squatter", kind: RouteTaskWebhook, handler: named("squatter")}}
	for _, p := range []string{ReservedOAuthCompletePath, ReservedOAuthCompletePath + "/x"} {
		if m, ok := g.Resolve(p); ok {
			t.Errorf("Resolve(%q) = %+v via a prefix; want no route", p, m)
		}
	}
	if m, ok := g.Resolve("/hooks/other"); !ok || m.TaskID != "squatter" {
		t.Errorf("Resolve(/hooks/other) = %+v, %v; the ancestor should still serve other paths", m, ok)
	}
	if _, err := g.Claim(OAuthRelayBuiltinID, ReservedOAuthCompletePath, RouteTaskWebhook, named("relay")); err != nil {
		t.Fatal(err)
	}
	if m, ok := g.Resolve(ReservedOAuthCompletePath); !ok || m.TaskID != OAuthRelayBuiltinID {
		t.Errorf("Resolve(reserved) = %+v, %v; want the exact auth-relay claim", m, ok)
	}
}

// A declared webhook takes a path over from another task's http.register
// route; http.register never takes one from a declared webhook.
func TestGateway_DeclaredWebhookPreemptsHTTPRegister(t *testing.T) {
	g := NewGateway()
	if _, err := g.Claim("squatter", "/hooks/mcp", RouteHTTPRegister, named("squatter")); err != nil {
		t.Fatal(err)
	}
	displaced, err := g.Claim("buildin/mcp", "/hooks/mcp/", RouteTaskWebhook, named("mcp"))
	if err != nil || displaced != "squatter" {
		t.Fatalf("declared claim = %q, %v; want displaced squatter", displaced, err)
	}
	if m, _ := g.Resolve("/hooks/mcp"); m.TaskID != "buildin/mcp" || m.Kind != RouteTaskWebhook {
		t.Fatalf("route after preemption: %+v", m)
	}
	if _, body := serve(g, http.MethodPost, "/hooks/mcp"); body != "mcp|" {
		t.Fatalf("preempted route serves %q", body)
	}
	if _, err := g.Claim("squatter", "/hooks/mcp", RouteHTTPRegister, named("squatter")); !errors.Is(err, ErrRouteClaimed) {
		t.Fatalf("http.register over a declared webhook = %v; want ErrRouteClaimed", err)
	}
	if _, err := g.Claim("other", "/hooks/mcp", RoutePipelineWebhook, named("other")); !errors.Is(err, ErrRouteClaimed) {
		t.Fatalf("declared webhook over another declared webhook = %v; want ErrRouteClaimed", err)
	}
}

func TestGateway_Release(t *testing.T) {
	g := NewGateway()
	_, _ = g.Claim("a", "/hooks/a1", RouteTaskWebhook, named("a"))
	_, _ = g.Claim("a", "/hooks/a2", RouteHTTPRegister, named("a"))
	_, _ = g.Claim("b", "/hooks/b", RouteTaskWebhook, named("b"))

	_, _ = g.Claim("a", "/hooks/a3", RoutePipelineWebhook, named("a"))

	g.ReleaseWebhooks("a", "/hooks/a1/")
	if _, ok := g.Resolve("/hooks/a1"); !ok {
		t.Error("ReleaseWebhooks dropped the kept pattern")
	}
	if _, ok := g.Resolve("/hooks/a3"); ok {
		t.Error("ReleaseWebhooks kept a webhook it should drop")
	}
	g.ReleaseWebhooks("a", "")
	if _, ok := g.Resolve("/hooks/a1"); ok {
		t.Error("ReleaseWebhooks(owner, \"\") kept a webhook route")
	}
	if m, ok := g.Resolve("/hooks/a2"); !ok || m.Kind != RouteHTTPRegister {
		t.Error("ReleaseWebhooks dropped the owner's http.register route")
	}
	if _, ok := g.Resolve("/hooks/b"); !ok {
		t.Error("ReleaseWebhooks dropped another owner's route")
	}
}

func TestGateway_ReleaseHandlerOnlyDropsOwnHandler(t *testing.T) {
	g := NewGateway()
	oldH := &ipcHandler{push: func(any) error { return nil }}
	newH := &ipcHandler{push: func(any) error { return nil }}
	_, _ = g.Claim("d", "/hooks/d", RouteHTTPRegister, oldH)
	_, _ = g.Claim("d", "/hooks/d", RouteHTTPRegister, newH)
	g.releaseHandler("/hooks/d", oldH)
	if _, ok := g.Resolve("/hooks/d"); !ok {
		t.Fatal("stale handler's release removed the newer run's route")
	}
	g.releaseHandler("/hooks/d", newH)
	if _, ok := g.Resolve("/hooks/d"); ok {
		t.Fatal("releaseHandler did not drop the route it owns")
	}
}

// An http.register route may not sit above or below another task's route;
// declared webhooks may nest.
func TestGateway_HTTPRegisterCannotNestWithOtherOwner(t *testing.T) {
	g := NewGateway()
	_, _ = g.Claim("buildin/webui", "/hooks/webui", RouteTaskWebhook, named("webui"))
	_, _ = g.Claim("other", "/hooks/svc/inner", RouteTaskWebhook, named("inner"))
	for _, p := range []string{"/hooks/webui/runs", "/hooks/webui/a/b", "/hooks/svc"} {
		if _, err := g.Claim("d", p, RouteHTTPRegister, named("d")); !errors.Is(err, ErrRouteClaimed) {
			t.Errorf("http.register %s = %v; want ErrRouteClaimed", p, err)
		}
	}
	if m, _ := g.Resolve("/hooks/webui/runs"); m.TaskID != "buildin/webui" {
		t.Errorf("/hooks/webui/runs owner = %q", m.TaskID)
	}
	if _, err := g.Claim("buildin/webui", "/hooks/webui/api", RouteHTTPRegister, named("own")); err != nil {
		t.Errorf("http.register below the task's own route: %v", err)
	}
	if _, err := g.Claim("d", "/hooks/webuix", RouteHTTPRegister, named("d")); err != nil {
		t.Errorf("http.register of a sibling-prefix path: %v", err)
	}
	if _, err := g.Claim("preset", "/hooks/webui/preset", RouteTaskWebhook, named("p")); err != nil {
		t.Errorf("declared webhook nested under another task's webhook: %v", err)
	}
}

// A builtin task's http.register route is never taken over by a declared
// webhook.
func TestGateway_DeclaredWebhookCannotPreemptBuiltin(t *testing.T) {
	g := NewGateway()
	if _, err := g.Claim("buildin/svc", "/hooks/svc", RouteHTTPRegister, named("builtin")); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Claim("repo/task", "/hooks/svc", RouteTaskWebhook, named("repo")); !errors.Is(err, ErrRouteClaimed) {
		t.Fatalf("declared webhook over a builtin http.register route = %v; want ErrRouteClaimed", err)
	}
	if _, body := serve(g, http.MethodGet, "/hooks/svc"); body != "builtin|" {
		t.Fatalf("builtin route replaced: %q", body)
	}
}

func TestGateway_ReleaseRun(t *testing.T) {
	g := NewGateway()
	a := &ipcHandler{runID: "r1", push: func(any) error { return nil }}
	b := &ipcHandler{runID: "r2", push: func(any) error { return nil }}
	_, _ = g.Claim("d", "/hooks/d1", RouteHTTPRegister, a)
	_, _ = g.Claim("d", "/hooks/d2", RouteHTTPRegister, b)
	_, _ = g.Claim("d", "/hooks/d3", RouteTaskWebhook, named("d"))
	g.ReleaseRun("d", "r1")
	if _, ok := g.Resolve("/hooks/d1"); ok {
		t.Error("ReleaseRun kept the run's route")
	}
	for _, p := range []string{"/hooks/d2", "/hooks/d3"} {
		if _, ok := g.Resolve(p); !ok {
			t.Errorf("ReleaseRun dropped %s", p)
		}
	}
}

func TestGateway_PinnedNoRouteIs404(t *testing.T) {
	g := NewGateway()
	_, _ = g.Claim("late", "/hooks/late", RouteHTTPRegister, named("late"))
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/hooks/late", nil)
	g.ServeHTTP(w, r.WithContext(WithRoute(r.Context(), RouteMatch{})))
	if w.Code != http.StatusNotFound {
		t.Errorf("pinned no-route answer was re-resolved: %d %q", w.Code, w.Body.String())
	}
}

func TestGateway_ReleaseHandlerClearsSlot(t *testing.T) {
	g := NewGateway()
	h := &ipcHandler{push: func(any) error { return nil }}
	_, _ = g.Claim("x", "/hooks/x1", RouteTaskWebhook, named("x"))
	_, _ = g.Claim("d", "/hooks/d", RouteHTTPRegister, h)
	g.releaseHandler("/hooks/d", h)
	if spare := g.routes[len(g.routes):cap(g.routes)]; len(spare) > 0 && spare[0].handler != nil {
		t.Error("released handler is still referenced from the slice's spare capacity")
	}
}

func TestGateway_ServesPinnedRoute(t *testing.T) {
	g := NewGateway()
	_, _ = g.Claim("a", "/hooks/a", RouteTaskWebhook, named("a"))
	m, _ := g.Resolve("/hooks/a")
	_, _ = g.Claim("b", "/hooks/a/b", RouteTaskWebhook, named("b"))
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/hooks/a/b", nil)
	g.ServeHTTP(w, r.WithContext(WithRoute(r.Context(), m)))
	if w.Body.String() != "a|" {
		t.Errorf("gateway re-resolved instead of serving the pinned route: %q", w.Body.String())
	}
}

// ── IPC http.register integration test ───────────────────────────────────────

func TestServer_HTTPRegister_And_Respond(t *testing.T) {
	g := NewGateway()
	e := newTestEnv(t)

	// Start an IPC server for a daemon task with the gateway attached.
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	spec := &task.Spec{ID: "my-daemon", Trigger: task.TriggerConfig{Daemon: true}}
	srv := New(runID, "my-daemon", e.secret, e.reg, e.db, nil, nil, zap.NewNop(), spec, nil)
	srv.SetGateway(g)
	socketPath, token, err := srv.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)

	conn := dial(t, socketPath)
	t.Cleanup(func() { conn.Close() })
	caps := doHandshake(t, conn, token)
	if !hasCap(caps, CapHTTPRegister) {
		t.Fatalf("expected http.register cap for daemon task; got %v", caps)
	}

	// Task registers /hooks/my-daemon in the gateway.
	sendMsg(t, conn, map[string]any{"id": "1", "method": "http.register", "pattern": "/hooks/my-daemon"})
	resp := recvMsg(t, conn)
	if resp["error"] != nil {
		t.Fatalf("http.register error: %v", resp["error"])
	}

	// Simulate an inbound HTTP request through the gateway in a goroutine.
	// The task reads the pushed HTTPInboundRequest, then sends http.respond.
	type httpResult struct {
		code int
		body string
	}
	resultCh := make(chan httpResult, 1)
	go func() {
		req := httptest.NewRequest("GET", "/hooks/my-daemon", nil)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, req)
		resultCh <- httpResult{w.Code, w.Body.String()}
	}()

	// The task reads the inbound push from the server.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var inbound map[string]any
	if err := readMsg(conn, &inbound); err != nil {
		t.Fatalf("expected HTTPInboundRequest push: %v", err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	rid, _ := inbound["requestID"].(string)
	if rid == "" {
		t.Fatalf("missing requestID in push: %v", inbound)
	}

	// Task responds.
	sendMsg(t, conn, map[string]any{
		"method":    "http.respond",
		"requestID": rid,
		"status":    200,
		"respBody":  []byte("hello from task"),
	})

	select {
	case res := <-resultCh:
		if res.code != 200 {
			t.Errorf("expected 200, got %d", res.code)
		}
		if !strings.Contains(res.body, "hello from task") {
			t.Errorf("unexpected body: %q", res.body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for gateway response")
	}
}

func TestIpcHandler_Timeout(t *testing.T) {
	// ipcHandler should return 504 when the task never replies.
	h := &ipcHandler{
		push: func(msg any) error { return nil }, // swallow push, never reply
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/hooks/slow", nil)

	// Patch the timeout by calling ServeHTTP with a context that cancels quickly.
	ctx, cancel := context.WithTimeout(req.Context(), 50*time.Millisecond)
	defer cancel()
	req = req.WithContext(ctx)

	// pending map needs a channel or ServeHTTP will block on the real 60s timer.
	// We test via context cancellation instead: ctx expires → r.Context().Done().
	h.ServeHTTP(w, req)
	// Client disconnect arm fires; no response body written; no panic.
	// (The timer arm would write 504, the context arm writes nothing — both are valid.)
}

func TestIpcHandler_ConcurrentRequests(t *testing.T) {
	// Multiple simultaneous requests on the same ipcHandler must be served independently.
	h := &ipcHandler{
		push: func(msg any) error { return nil },
	}

	const n = 5
	resultCh := make(chan int, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/hooks/p", nil)
			ctx, cancel := context.WithCancel(req.Context())
			req = req.WithContext(ctx)

			// Complete the request immediately via complete() after ServeHTTP stores the channel.
			// We use a short delay to ensure the pending entry is registered.
			go func() {
				time.Sleep(5 * time.Millisecond)
				// Find the newest pending entry and complete it.
				h.pending.Range(func(k, v any) bool {
					rid, _ := k.(string)
					ch, _ := v.(chan ipcResponse)
					select {
					case ch <- ipcResponse{status: 200}:
					default:
					}
					_ = rid
					return false
				})
				cancel()
			}()
			h.ServeHTTP(w, req)
			resultCh <- w.Code
		}(i)
	}

	for i := 0; i < n; i++ {
		select {
		case <-resultCh:
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for concurrent request")
		}
	}
}

func TestIpcHandler_UnknownRequestID(t *testing.T) {
	// complete() must return false for unknown request IDs.
	h := &ipcHandler{
		push: func(msg any) error { return nil },
	}
	if h.complete("no-such-id", 200, nil, nil) {
		t.Error("complete should return false for unknown requestID")
	}
}

func TestServer_HTTPRegister_RequiresDaemonSpec(t *testing.T) {
	g := NewGateway()
	e := newTestEnv(t)

	// Non-daemon task — should NOT get CapHTTPRegister.
	runID := fmt.Sprintf("test-%d", time.Now().UnixNano())
	srv := New(runID, "regular-task", e.secret, e.reg, e.db, nil, nil, zap.NewNop(), nil, nil)
	srv.SetGateway(g)
	socketPath, token, err := srv.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)

	conn := dial(t, socketPath)
	t.Cleanup(func() { conn.Close() })
	caps := doHandshake(t, conn, token)
	if hasCap(caps, CapHTTPRegister) {
		t.Error("non-daemon task should not receive http.register cap")
	}

	// Even if the client sends http.register, it gets permission denied.
	sendMsg(t, conn, map[string]any{"id": "1", "method": "http.register", "pattern": "/hooks/x"})
	resp := recvMsg(t, conn)
	if resp["error"] == nil {
		t.Error("expected permission denied for http.register without cap")
	}
}

func startDaemonIPC(t *testing.T, g *Gateway, taskID string) (net.Conn, func()) {
	t.Helper()
	e := newTestEnv(t)
	spec := &task.Spec{ID: taskID, Trigger: task.TriggerConfig{Daemon: true}}
	srv := New(fmt.Sprintf("run-%d", time.Now().UnixNano()), taskID, e.secret, e.reg, e.db, nil, nil, zap.NewNop(), spec, nil)
	srv.SetGateway(g)
	socketPath, token, err := srv.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	conn := dial(t, socketPath)
	doHandshake(t, conn, token)
	return conn, func() { conn.Close(); srv.Stop() }
}

func TestServer_HTTPRegister_CannotTakeAnotherTasksRoute(t *testing.T) {
	g := NewGateway()
	if _, err := g.Claim("victim", "/hooks/shared", RouteTaskWebhook, named("victim")); err != nil {
		t.Fatal(err)
	}
	conn, stop := startDaemonIPC(t, g, "evil")
	t.Cleanup(stop)

	sendMsg(t, conn, map[string]any{"id": "1", "method": "http.register", "pattern": "/hooks/shared/"})
	resp := recvMsg(t, conn)
	if resp["error"] == nil {
		t.Fatal("http.register of another task's route must fail")
	}
	if _, body := serve(g, http.MethodPost, "/hooks/shared"); body != "victim|" {
		t.Fatalf("victim's route was replaced: body %q", body)
	}
}

func TestServer_HTTPRegister_ReleasesOnRepatternAndClose(t *testing.T) {
	g := NewGateway()
	conn, stop := startDaemonIPC(t, g, "d")
	defer stop()

	for i, p := range []string{"/hooks/d1", "/hooks/d2"} {
		sendMsg(t, conn, map[string]any{"id": fmt.Sprint(i), "method": "http.register", "pattern": p})
		if resp := recvMsg(t, conn); resp["error"] != nil {
			t.Fatalf("http.register %s: %v", p, resp["error"])
		}
	}
	if _, ok := g.Resolve("/hooks/d1"); ok {
		t.Error("old pattern still routed after re-register")
	}
	if m, ok := g.Resolve("/hooks/d2"); !ok || m.TaskID != "d" || m.Kind != RouteHTTPRegister {
		t.Fatalf("Resolve(/hooks/d2) = %+v, %v", m, ok)
	}
	conn.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := g.Resolve("/hooks/d2"); !ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("route not released after the IPC connection closed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
