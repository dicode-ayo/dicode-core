package globals

import (
	"encoding/json"

	"github.com/dop251/goja"
)

// OutputResult is what a task returns when using the output global.
type OutputResult struct {
	ContentType string
	Content     string
	Data        interface{} // for chain compatibility; nil if not set
}

// InjectOutput sets up the `output` global and returns a pointer that will
// hold the result once the task calls one of its methods.
//
// If the task returns a plain value instead of calling output.*, the runtime
// captures it separately via the script return value.
func InjectOutput(vm *goja.Runtime) *OutputResult {
	result := &OutputResult{}

	obj := vm.NewObject()

	_ = obj.Set("html", func(call goja.FunctionCall) goja.Value {
		content := ""
		if len(call.Arguments) > 0 {
			content = call.Arguments[0].String()
		}
		result.ContentType = "text/html"
		result.Content = content
		if len(call.Arguments) > 1 {
			if opts, ok := call.Arguments[1].(*goja.Object); ok {
				if d := opts.Get("data"); d != nil && !goja.IsUndefined(d) {
					result.Data = d.Export()
				}
			}
		}
		return vm.ToValue(result.asJS())
	})

	_ = obj.Set("text", func(call goja.FunctionCall) goja.Value {
		content := ""
		if len(call.Arguments) > 0 {
			content = call.Arguments[0].String()
		}
		result.ContentType = "text/plain"
		result.Content = content
		return vm.ToValue(result.asJS())
	})

	_ = obj.Set("image", func(call goja.FunctionCall) goja.Value {
		mime := "image/png"
		if len(call.Arguments) > 0 {
			mime = call.Arguments[0].String()
		}
		data := ""
		if len(call.Arguments) > 1 {
			data = call.Arguments[1].String()
		}
		result.ContentType = mime
		result.Content = data
		return vm.ToValue(result.asJS())
	})

	_ = obj.Set("file", func(call goja.FunctionCall) goja.Value {
		name := "file"
		if len(call.Arguments) > 0 {
			name = call.Arguments[0].String()
		}
		content := ""
		if len(call.Arguments) > 1 {
			content = call.Arguments[1].String()
		}
		mime := "application/octet-stream"
		if len(call.Arguments) > 2 {
			mime = call.Arguments[2].String()
		}
		result.ContentType = mime
		result.Content = content
		result.Data = map[string]string{"filename": name}
		return vm.ToValue(result.asJS())
	})

	vm.Set("output", obj)
	return result
}

func (r *OutputResult) asJS() map[string]interface{} {
	return map[string]interface{}{
		"contentType": r.ContentType,
		"content":     r.Content,
		"data":        r.Data,
	}
}

// IsSet reports whether the task used the output global.
func (r *OutputResult) IsSet() bool {
	return r.ContentType != ""
}

// ToJSON serialises the result for storage in sqlite.
func (r *OutputResult) ToJSON() (string, error) {
	b, err := json.Marshal(r)
	return string(b), err
}
