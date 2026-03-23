package globals

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dop251/goja"
)

// HTTPInterceptor can intercept outbound HTTP requests (used in dry-run / test modes).
type HTTPInterceptor func(method, url string, body []byte) (status int, respBody []byte, intercepted bool)

// InjectHTTP sets up the `http` global.
// loop is used to schedule promise resolution back onto the event loop from goroutines.
// If interceptor is non-nil, it is called before each real request.
func InjectHTTP(vm *goja.Runtime, interceptor HTTPInterceptor) {
	client := &http.Client{Timeout: 30 * time.Second}

	do := func(method, url string, opts *goja.Object) (map[string]interface{}, error) {
		var reqBody []byte
		var headers map[string]string
		var timeoutDur time.Duration

		if opts != nil {
			if b := opts.Get("body"); b != nil && !goja.IsUndefined(b) && !goja.IsNull(b) {
				switch v := b.Export().(type) {
				case string:
					reqBody = []byte(v)
				default:
					var err error
					reqBody, err = json.Marshal(v)
					if err != nil {
						return nil, fmt.Errorf("marshal body: %w", err)
					}
				}
			}
			if h := opts.Get("headers"); h != nil && !goja.IsUndefined(h) {
				if hmap, ok := h.Export().(map[string]interface{}); ok {
					headers = make(map[string]string, len(hmap))
					for k, v := range hmap {
						headers[k] = fmt.Sprint(v)
					}
				}
			}
			if to := opts.Get("timeout"); to != nil && !goja.IsUndefined(to) {
				d, err := time.ParseDuration(to.String())
				if err == nil {
					timeoutDur = d
				}
			}
		}

		if interceptor != nil {
			status, body, intercepted := interceptor(method, url, reqBody)
			if intercepted {
				return buildResponse(status, body)
			}
		}

		c := client
		if timeoutDur > 0 {
			c = &http.Client{Timeout: timeoutDur}
		}

		req, err := http.NewRequest(method, url, bytes.NewReader(reqBody))
		if err != nil {
			return nil, err
		}
		if len(reqBody) > 0 {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		resp, err := c.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		return buildResponse(resp.StatusCode, body)
	}

	makeMethod := func(method string) func(goja.FunctionCall) goja.Value {
		return func(call goja.FunctionCall) goja.Value {
			if len(call.Arguments) == 0 {
				panic(vm.ToValue("http." + strings.ToLower(method) + " requires a URL"))
			}
			url := call.Arguments[0].String()
			var opts *goja.Object
			if len(call.Arguments) > 1 {
				if o, ok := call.Arguments[1].(*goja.Object); ok {
					opts = o
				}
			}

			// Perform the HTTP call synchronously and resolve the promise immediately.
			// This keeps everything on the event loop and avoids goroutine timing issues.
			result, err := do(method, url, opts)
			promise, resolve, reject := vm.NewPromise()
			if err != nil {
				_ = reject(err.Error())
			} else {
				_ = resolve(vm.ToValue(result))
			}
			return vm.ToValue(promise)
		}
	}

	obj := vm.NewObject()
	_ = obj.Set("get", makeMethod("GET"))
	_ = obj.Set("post", makeMethod("POST"))
	_ = obj.Set("put", makeMethod("PUT"))
	_ = obj.Set("patch", makeMethod("PATCH"))
	_ = obj.Set("delete", makeMethod("DELETE"))

	vm.Set("http", obj)
}

func buildResponse(status int, body []byte) (map[string]interface{}, error) {
	result := map[string]interface{}{
		"status": status,
	}
	var parsed interface{}
	if json.Unmarshal(body, &parsed) == nil {
		result["body"] = parsed
	} else {
		result["body"] = string(body)
	}
	return result, nil
}
