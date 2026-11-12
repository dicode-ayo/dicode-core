package trigger

import "github.com/dicode/dicode/pkg/ipc"

// routeOwner returns the task that claimed exactly pattern, or "".
func (e *Engine) routeOwner(pattern string) string {
	m, ok := e.routes.Resolve(ipc.NormalizePattern(pattern))
	if !ok || m.AssetPath != "" || m.Pattern != ipc.NormalizePattern(pattern) {
		return ""
	}
	return m.TaskID
}
