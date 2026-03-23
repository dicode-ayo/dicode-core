package globals

import (
	"github.com/dop251/goja"
)

// InjectEnv sets up the `env` global backed by the resolved secrets map.
// env.get("KEY") returns the resolved secret value or "" if not set.
func InjectEnv(vm *goja.Runtime, resolved map[string]string) {
	obj := vm.NewObject()

	_ = obj.Set("get", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			return vm.ToValue("")
		}
		key := call.Arguments[0].String()
		return vm.ToValue(resolved[key])
	})

	vm.Set("env", obj)
}
