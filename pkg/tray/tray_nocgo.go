//go:build !cgo

package tray

import (
	"context"

	"go.uber.org/zap"
)

// Run is a no-op when CGO is disabled (cross-compiled build without systray support).
func Run(_ context.Context, _ context.CancelFunc, _ int, _ *zap.Logger) {}
