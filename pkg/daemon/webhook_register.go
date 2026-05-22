package daemon

import (
	"net/http"
	"sync"

	"github.com/dicode/dicode/pkg/ipc"
	"github.com/dicode/dicode/pkg/task"
)

// registerGatewayWebhook claims the daemon-level HTTP gateway route for a task
// or pipeline that declares a webhook trigger, recording the path under the
// task ID so OnUnregister can deregister it later.
//
// This is kind-aware: both kind: Task (*task.Spec) and kind: PipelineTask
// (*task.PipelineTask) can carry a webhook trigger. The engine already wires
// the routing side for both (registerWebhook/registerWebhookPath populate the
// engine's internal webhooks map, and WebhookHandler dispatches pipelines via
// GetKinded → handlePipelineWebhook). What was missing for pipelines was THIS:
// the daemon-level gateway.Register that puts the path on the HTTP surface so a
// POST actually reaches webhookH. Without it, a pipeline's webhook returned 404
// before the handler ran (GAP 1).
//
// webhookH is the same gateway handler used for kind: Task — it routes into the
// engine's WebhookHandler, which resolves the kind and fires the right path.
//
// No-op for kinds with no webhook trigger (manual/cron/chain), and for unknown
// kinds.
func registerGatewayWebhook(
	gateway *ipc.Gateway,
	webhookPaths map[string]string,
	webhookMu *sync.Mutex,
	webhookH http.Handler,
	k task.Kinded,
) {
	var id, path string
	switch v := k.(type) {
	case *task.Spec:
		id, path = v.ID, v.Trigger.Webhook
	case *task.PipelineTask:
		id, path = v.ID, v.Trigger.Webhook
	default:
		return
	}
	if path == "" {
		return
	}
	gateway.Register(path, webhookH)
	webhookMu.Lock()
	webhookPaths[id] = path
	webhookMu.Unlock()
}
