package globals

import (
	"github.com/dop251/goja"
	"github.com/dicode/dicode/pkg/task"
)

// InjectParams sets up the `params` global.
// Values come from task.yaml defaults merged with run-time overrides.
func InjectParams(vm *goja.Runtime, spec *task.Spec, overrides map[string]string) {
	// Build merged map: spec defaults < overrides.
	merged := make(map[string]string)
	for _, p := range spec.Params {
		if p.Default != "" {
			merged[p.Name] = p.Default
		}
	}
	for k, v := range overrides {
		merged[k] = v
	}

	obj := vm.NewObject()

	_ = obj.Set("get", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			return goja.Undefined()
		}
		key := call.Arguments[0].String()
		if v, ok := merged[key]; ok {
			return vm.ToValue(v)
		}
		return goja.Undefined()
	})

	_ = obj.Set("all", func(call goja.FunctionCall) goja.Value {
		return vm.ToValue(merged)
	})

	vm.Set("params", obj)
}
