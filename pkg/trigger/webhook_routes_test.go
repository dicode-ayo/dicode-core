package trigger

import (
	"testing"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// TestResolveWebhookAuth_RouteTable: (registered routes, request path) →
// (owning task, asset path, auth posture), across engine-claimed webhooks and
// a daemon task's http.register route.
func TestResolveWebhookAuth_RouteTable(t *testing.T) {
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)
	eng := New(reg, nil, zap.NewNop())

	webhook := func(id, path string, mode task.WebhookAuthMode, secret string) {
		s := &task.Spec{ID: id, Name: id, Enabled: true, Runtime: task.RuntimeDeno,
			Trigger: task.TriggerConfig{Webhook: path, WebhookAuth: mode, WebhookSecret: secret}}
		if err := reg.Register(s); err != nil {
			t.Fatal(err)
		}
		if err := eng.Register(s); err != nil {
			t.Fatal(err)
		}
	}
	webhook("ai", "/hooks/ai", task.WebhookAuthNone, "")
	webhook("dicodai", "/hooks/ai/dicodai", task.WebhookAuthSession, "")
	webhook("slash", "/hooks/slash/", task.WebhookAuthAny, "s3cret")

	daemon := &task.Spec{ID: "daemon", Name: "daemon", Enabled: true, Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Daemon: true, WebhookAuth: task.WebhookAuthAny, WebhookSecret: "s3cret"}}
	if err := reg.Register(daemon); err != nil {
		t.Fatal(err)
	}
	for owner, p := range map[string]string{"daemon": "/hooks/daemon", "gone": "/hooks/gone"} {
		if _, err := eng.routes.Claim(owner, p, ipc.RouteHTTPRegister, nil); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		path      string
		matched   bool
		deny      bool
		taskID    string
		assetPath string
		mode      task.WebhookAuthMode
		hasSecret bool
	}{
		{path: "/hooks/ai", matched: true, taskID: "ai"},
		{path: "/hooks/ai/app.js", matched: true, taskID: "ai", assetPath: "app.js"},
		// Sibling prefix: the protected preset wins for its own subtree.
		{path: "/hooks/ai/dicodai", matched: true, taskID: "dicodai", mode: task.WebhookAuthSession},
		{path: "/hooks/ai/dicodai/app.js", matched: true, taskID: "dicodai", assetPath: "app.js", mode: task.WebhookAuthSession},
		{path: "/hooks/ai/dicodaix", matched: true, taskID: "ai", assetPath: "dicodaix"},
		// Trailing-slash pattern.
		{path: "/hooks/slash", matched: true, taskID: "slash", mode: task.WebhookAuthAny, hasSecret: true},
		{path: "/hooks/slash/sub/x.css", matched: true, taskID: "slash", assetPath: "sub/x.css", mode: task.WebhookAuthAny, hasSecret: true},
		// Gateway-only route: inherits the daemon's trigger.auth; no HMAC on it.
		{path: "/hooks/daemon", matched: true, taskID: "daemon", mode: task.WebhookAuthAny},
		{path: "/hooks/daemon/api", matched: true, taskID: "daemon", assetPath: "api", mode: task.WebhookAuthAny},
		// A route whose owner is not in the registry fails closed.
		{path: "/hooks/gone", matched: true, deny: true, taskID: "gone"},
		{path: "/hooks/none"},
	}
	for _, tc := range cases {
		got := eng.ResolveWebhookAuth(tc.path)
		if got.Matched != tc.matched || got.Deny != tc.deny || got.Route.TaskID != tc.taskID ||
			got.Route.AssetPath != tc.assetPath || got.IsAsset != (tc.assetPath != "") ||
			got.Mode != tc.mode || got.HasSecret != tc.hasSecret {
			t.Errorf("ResolveWebhookAuth(%q) = {matched:%v deny:%v task:%q asset:%q mode:%q secret:%v}; want {matched:%v deny:%v task:%q asset:%q mode:%q secret:%v}",
				tc.path, got.Matched, got.Deny, got.Route.TaskID, got.Route.AssetPath, got.Mode, got.HasSecret,
				tc.matched, tc.deny, tc.taskID, tc.assetPath, tc.mode, tc.hasSecret)
		}
	}
}

func newRouteEngine(t *testing.T) (*Engine, *registry.Registry) {
	t.Helper()
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)
	return New(reg, nil, zap.NewNop()), reg
}

func registerWebhookTask(t *testing.T, eng *Engine, reg *registry.Registry, id, path string) {
	t.Helper()
	s := &task.Spec{ID: id, Name: id, Enabled: true, Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Webhook: path}}
	_ = reg.Register(s)
	if err := eng.Register(s); err != nil {
		t.Fatal(err)
	}
}

// TestRegister_WebhookPreemptsHTTPRegisterRoute: a task declaring a webhook
// takes the path over from a daemon task that squatted it via http.register.
func TestRegister_WebhookPreemptsHTTPRegisterRoute(t *testing.T) {
	eng, reg := newRouteEngine(t)
	if _, err := eng.routes.Claim("squatter", "/hooks/mcp", ipc.RouteHTTPRegister, nil); err != nil {
		t.Fatal(err)
	}
	registerWebhookTask(t, eng, reg, "buildin/mcp", "/hooks/mcp")
	if got := eng.routeOwner("/hooks/mcp"); got != "buildin/mcp" {
		t.Fatalf("owner = %q; want buildin/mcp", got)
	}
	if _, err := eng.routes.Claim("squatter", "/hooks/mcp", ipc.RouteHTTPRegister, nil); err == nil {
		t.Fatal("http.register re-took a declared webhook route")
	}
}

// TestRegister_KeepsTasksHTTPRegisterRoutes: re-registering or unregistering
// a task releases only its webhook routes; its http.register routes belong to
// the IPC connection that claimed them.
func TestRegister_KeepsTasksHTTPRegisterRoutes(t *testing.T) {
	eng, reg := newRouteEngine(t)
	if _, err := eng.routes.Claim("t", "/hooks/t-api", ipc.RouteHTTPRegister, nil); err != nil {
		t.Fatal(err)
	}
	registerWebhookTask(t, eng, reg, "t", "/hooks/t")
	registerWebhookTask(t, eng, reg, "t", "/hooks/t2")
	if m, ok := eng.routes.Resolve("/hooks/t-api"); !ok || m.TaskID != "t" {
		t.Fatal("re-register dropped the task's http.register route")
	}
	if eng.routeOwner("/hooks/t") != "" {
		t.Fatal("re-register kept the old webhook route")
	}
	eng.Unregister("t")
	if _, ok := eng.routes.Resolve("/hooks/t-api"); !ok {
		t.Fatal("Unregister dropped the task's http.register route")
	}
	if eng.routeOwner("/hooks/t2") != "" {
		t.Fatal("Unregister kept the webhook route")
	}
}
