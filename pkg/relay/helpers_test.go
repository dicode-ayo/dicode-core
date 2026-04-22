package relay

import (
	"go.uber.org/zap"
)

func noopLogger() *zap.Logger {
	return zap.NewNop()
}
