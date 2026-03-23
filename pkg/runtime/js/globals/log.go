package globals

import (
	"github.com/dop251/goja"
	"go.uber.org/zap"
)

// LogSink collects log entries emitted by a running task.
type LogSink interface {
	AppendLog(level, msg string)
}

// InjectLog sets up the `log` global with info/warn/error/debug methods.
func InjectLog(vm *goja.Runtime, sink LogSink, zapLog *zap.Logger) {
	obj := vm.NewObject()

	mkFn := func(level string) func(goja.FunctionCall) goja.Value {
		return func(call goja.FunctionCall) goja.Value {
			msg := ""
			if len(call.Arguments) > 0 {
				msg = call.Arguments[0].String()
			}
			sink.AppendLog(level, msg)
			switch level {
			case "warn":
				zapLog.Warn(msg)
			case "error":
				zapLog.Error(msg)
			case "debug":
				zapLog.Debug(msg)
			default:
				zapLog.Info(msg)
			}
			return goja.Undefined()
		}
	}

	_ = obj.Set("info", mkFn("info"))
	_ = obj.Set("warn", mkFn("warn"))
	_ = obj.Set("error", mkFn("error"))
	_ = obj.Set("debug", mkFn("debug"))

	vm.Set("log", obj)
}
