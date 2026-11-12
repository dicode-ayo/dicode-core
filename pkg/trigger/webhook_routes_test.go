package trigger

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dicode/dicode/pkg/db"
	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
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
		if got.Matched() != tc.matched || got.Deny != tc.deny || got.Route.TaskID != tc.taskID ||
			got.Route.AssetPath != tc.assetPath || got.IsAsset != (tc.assetPath != "") ||
			got.Mode != tc.mode || got.HasSecret != tc.hasSecret {
			t.Errorf("ResolveWebhookAuth(%q) = {matched:%v deny:%v task:%q asset:%q mode:%q secret:%v}; want {matched:%v deny:%v task:%q asset:%q mode:%q secret:%v}",
				tc.path, got.Matched(), got.Deny, got.Route.TaskID, got.Route.AssetPath, got.Mode, got.HasSecret,
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

// TestRegisterAdmitted_SkipsHeldTasks: the boot-time registration pass leaves a
// task the fire guard holds without a route.
func TestRegisterAdmitted_SkipsHeldTasks(t *testing.T) {
	eng, reg := newRouteEngine(t)
	eng.SetFireGuard(func(id string) error {
		if id == "held" {
			return errors.New("pending approval")
		}
		return nil
	})
	for id, p := range map[string]string{"held": "/hooks/held", "ok": "/hooks/ok"} {
		if err := reg.Register(&task.Spec{ID: id, Name: id, Enabled: true, Runtime: task.RuntimeDeno,
			Trigger: task.TriggerConfig{Webhook: p}}); err != nil {
			t.Fatal(err)
		}
	}
	eng.registerAdmitted()
	if got := eng.routeOwner("/hooks/ok"); got != "ok" {
		t.Fatalf("admitted task route owner = %q", got)
	}
	w := httptest.NewRecorder()
	eng.routes.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/hooks/held", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("held task's webhook: got %d, want 404", w.Code)
	}
}

// TestWebhookUI_HeldTaskServesNothing: a routed task the fire guard holds
// serves neither its index.html nor its assets.
func TestWebhookUI_HeldTaskServesNothing(t *testing.T) {
	eng, reg := newRouteEngine(t)
	dir := t.TempDir()
	for name, body := range map[string]string{"index.html": "<html>secret ui</html>", "app.js": "secret()"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := &task.Spec{ID: "ui", Name: "ui", Enabled: true, Runtime: task.RuntimeDeno, TaskDir: dir,
		Trigger: task.TriggerConfig{Webhook: "/hooks/ui"}}
	_ = reg.Register(s)
	if err := eng.Register(s); err != nil {
		t.Fatal(err)
	}
	var held atomic.Bool
	eng.SetFireGuard(func(string) error {
		if held.Load() {
			return errors.New("pending approval")
		}
		return nil
	})
	for _, p := range []string{"/hooks/ui", "/hooks/ui/app.js", "/hooks/ui/deep/route"} {
		w := httptest.NewRecorder()
		eng.routes.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("admitted GET %s: %d", p, w.Code)
		}
	}
	held.Store(true)
	for _, p := range []string{"/hooks/ui", "/hooks/ui/app.js", "/hooks/ui/deep/route"} {
		w := httptest.NewRecorder()
		eng.routes.ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "secret") {
			t.Errorf("held GET %s: %d %q, want 404", p, w.Code, w.Body.String())
		}
	}
}

// TestRegister_FailedReRegistrationReleasesRoute: a re-registration that fails
// validation leaves no route serving the rejected spec.
func TestRegister_FailedReRegistrationReleasesRoute(t *testing.T) {
	eng, reg := newRouteEngine(t)
	registerWebhookTask(t, eng, reg, "a", "/hooks/a")
	cyclic := &task.Spec{ID: "a", Name: "a", Enabled: true, Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Webhook: "/hooks/a", Chain: &task.ChainTrigger{From: "a"}}}
	_ = reg.Register(cyclic)
	if err := eng.Register(cyclic); err == nil {
		t.Fatal("expected a success-chain cycle error")
	}
	if got := eng.routeOwner("/hooks/a"); got != "" {
		t.Fatalf("rejected spec still holds its route (owner %q)", got)
	}

	registerWebhookTask(t, eng, reg, "s", "/hooks/s-stage")
	pipe := &task.PipelineTask{APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "p", Name: "P", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Webhook: "/hooks/p"}, Stages: []task.Stage{{Task: "s"}}}
	_ = reg.Register(pipe)
	if err := eng.Register(pipe); err != nil {
		t.Fatal(err)
	}
	bad := *pipe
	bad.Subtype = "no-such-subtype"
	if err := eng.Register(&bad); err == nil {
		t.Fatal("expected a pipeline validation error")
	}
	if got := eng.routeOwner("/hooks/p"); got != "" {
		t.Fatalf("rejected pipeline still holds its route (owner %q)", got)
	}
}

// TestClaimWebhook_RepeatRejectionLoggedOnce: a deferred pipeline retried on
// every stage registration warns about a held path once.
func TestClaimWebhook_RepeatRejectionLoggedOnce(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	d, err := db.Open(db.Config{Type: "sqlite", Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	reg := registry.New(d)
	eng := New(reg, nil, zap.New(core))
	registerWebhookTask(t, eng, reg, "holder", "/hooks/shared")
	pipe := &task.PipelineTask{APIVersion: "dicode/v1", Kind: task.KindPipelineTask,
		ID: "p", Name: "P", Subtype: "sequential", Enabled: true,
		Trigger: task.PipelineTrigger{Webhook: "/hooks/shared"}, Stages: []task.Stage{{Task: "missing"}}}
	_ = reg.Register(pipe)
	if err := eng.Register(pipe); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"x1", "x2", "x3"} {
		registerWebhookTask(t, eng, reg, id, "/hooks/"+id)
	}
	warns := logs.FilterMessage("rejecting webhook path").FilterLevelExact(zap.WarnLevel).Len()
	debugs := logs.FilterMessage("rejecting webhook path").FilterLevelExact(zap.DebugLevel).Len()
	if warns != 1 || debugs < 3 {
		t.Fatalf("rejecting webhook path: %d WARN, %d DEBUG; want 1 WARN then DEBUG", warns, debugs)
	}
}
