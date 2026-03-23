package globals

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/dop251/goja"
	"github.com/dicode/dicode/pkg/task"
)

// InjectFS sets up the `fs` global. Only called when task.yaml declares fs: entries.
// All paths are validated against declared entries before any operation.
func InjectFS(vm *goja.Runtime, entries []task.FSEntry) {
	obj := vm.NewObject()

	check := func(path string, needWrite bool) (string, error) {
		abs, err := filepath.Abs(expandHome(path))
		if err != nil {
			return "", fmt.Errorf("invalid path: %w", err)
		}
		resolved, err := resolveSymlinks(abs)
		if err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("resolve path: %w", err)
		}
		if os.IsNotExist(err) {
			resolved = abs // path doesn't exist yet (write target)
		}
		for _, e := range entries {
			allowed, _ := filepath.Abs(expandHome(e.Path))
			if strings.HasPrefix(resolved, allowed+string(os.PathSeparator)) || resolved == allowed {
				if needWrite && e.Permission == "r" {
					return "", &PermissionError{Path: path, Need: "w", Have: "r"}
				}
				return resolved, nil
			}
		}
		return "", &PermissionError{Path: path, Need: "r", Have: "none"}
	}

	promise := func(fn func() (interface{}, error)) *goja.Promise {
		p, resolve, reject := vm.NewPromise()
		go func() {
			v, err := fn()
			if err != nil {
				reject(vm.ToValue(err.Error()))
			} else {
				resolve(vm.ToValue(v))
			}
		}()
		return p
	}

	_ = obj.Set("read", func(call goja.FunctionCall) goja.Value {
		path := argStr(call, 0)
		return vm.ToValue(promise(func() (interface{}, error) {
			p, err := check(path, false)
			if err != nil {
				return nil, err
			}
			b, err := os.ReadFile(p)
			return string(b), err
		}))
	})

	_ = obj.Set("readJSON", func(call goja.FunctionCall) goja.Value {
		path := argStr(call, 0)
		return vm.ToValue(promise(func() (interface{}, error) {
			p, err := check(path, false)
			if err != nil {
				return nil, err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return nil, err
			}
			var v interface{}
			return v, json.Unmarshal(b, &v)
		}))
	})

	_ = obj.Set("write", func(call goja.FunctionCall) goja.Value {
		path, content := argStr(call, 0), argStr(call, 1)
		return vm.ToValue(promise(func() (interface{}, error) {
			p, err := check(path, true)
			if err != nil {
				return nil, err
			}
			return nil, os.WriteFile(p, []byte(content), 0644)
		}))
	})

	_ = obj.Set("writeJSON", func(call goja.FunctionCall) goja.Value {
		path := argStr(call, 0)
		return vm.ToValue(promise(func() (interface{}, error) {
			p, err := check(path, true)
			if err != nil {
				return nil, err
			}
			var data interface{}
			if len(call.Arguments) > 1 {
				data = call.Arguments[1].Export()
			}
			b, err := json.MarshalIndent(data, "", "  ")
			if err != nil {
				return nil, err
			}
			return nil, os.WriteFile(p, b, 0644)
		}))
	})

	_ = obj.Set("append", func(call goja.FunctionCall) goja.Value {
		path, content := argStr(call, 0), argStr(call, 1)
		return vm.ToValue(promise(func() (interface{}, error) {
			p, err := check(path, true)
			if err != nil {
				return nil, err
			}
			f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
			if err != nil {
				return nil, err
			}
			defer f.Close()
			_, err = f.WriteString(content)
			return nil, err
		}))
	})

	_ = obj.Set("exists", func(call goja.FunctionCall) goja.Value {
		path := argStr(call, 0)
		return vm.ToValue(promise(func() (interface{}, error) {
			p, err := check(path, false)
			if err != nil {
				return false, nil // path outside allowed — treat as not found
			}
			_, err = os.Stat(p)
			return !os.IsNotExist(err), nil
		}))
	})

	_ = obj.Set("stat", func(call goja.FunctionCall) goja.Value {
		path := argStr(call, 0)
		return vm.ToValue(promise(func() (interface{}, error) {
			p, err := check(path, false)
			if err != nil {
				return nil, err
			}
			info, err := os.Stat(p)
			if err != nil {
				return nil, err
			}
			return map[string]interface{}{
				"name":     info.Name(),
				"size":     info.Size(),
				"isDir":    info.IsDir(),
				"modified": info.ModTime().UnixMilli(),
			}, nil
		}))
	})

	_ = obj.Set("list", func(call goja.FunctionCall) goja.Value {
		path := argStr(call, 0)
		return vm.ToValue(promise(func() (interface{}, error) {
			p, err := check(path, false)
			if err != nil {
				return nil, err
			}
			entries, err := os.ReadDir(p)
			if err != nil {
				return nil, err
			}
			result := make([]map[string]interface{}, 0, len(entries))
			for _, e := range entries {
				info, _ := e.Info()
				var size int64
				if info != nil {
					size = info.Size()
				}
				result = append(result, map[string]interface{}{
					"name":  e.Name(),
					"path":  filepath.Join(p, e.Name()),
					"isDir": e.IsDir(),
					"size":  size,
				})
			}
			return result, nil
		}))
	})

	_ = obj.Set("glob", func(call goja.FunctionCall) goja.Value {
		pattern := argStr(call, 0)
		return vm.ToValue(promise(func() (interface{}, error) {
			// Validate the base dir of the pattern.
			base := filepath.Dir(pattern)
			if _, err := check(base, false); err != nil {
				return nil, err
			}
			matches, err := filepath.Glob(expandHome(pattern))
			return matches, err
		}))
	})

	_ = obj.Set("mkdir", func(call goja.FunctionCall) goja.Value {
		path := argStr(call, 0)
		return vm.ToValue(promise(func() (interface{}, error) {
			p, err := check(path, true)
			if err != nil {
				return nil, err
			}
			return nil, os.MkdirAll(p, 0755)
		}))
	})

	_ = obj.Set("copy", func(call goja.FunctionCall) goja.Value {
		src, dst := argStr(call, 0), argStr(call, 1)
		return vm.ToValue(promise(func() (interface{}, error) {
			sp, err := check(src, false)
			if err != nil {
				return nil, err
			}
			dp, err := check(dst, true)
			if err != nil {
				return nil, err
			}
			return nil, copyFile(sp, dp)
		}))
	})

	_ = obj.Set("move", func(call goja.FunctionCall) goja.Value {
		src, dst := argStr(call, 0), argStr(call, 1)
		return vm.ToValue(promise(func() (interface{}, error) {
			sp, err := check(src, true)
			if err != nil {
				return nil, err
			}
			dp, err := check(dst, true)
			if err != nil {
				return nil, err
			}
			return nil, os.Rename(sp, dp)
		}))
	})

	_ = obj.Set("delete", func(call goja.FunctionCall) goja.Value {
		path := argStr(call, 0)
		return vm.ToValue(promise(func() (interface{}, error) {
			p, err := check(path, true)
			if err != nil {
				return nil, err
			}
			return nil, os.Remove(p)
		}))
	})

	vm.Set("fs", obj)
}

// PermissionError is returned when a task tries to access a path it hasn't declared.
type PermissionError struct {
	Path string
	Need string
	Have string
}

func (e *PermissionError) Error() string {
	return fmt.Sprintf("fs: permission denied for %q (need %s, have %s)", e.Path, e.Need, e.Have)
}

func argStr(call goja.FunctionCall, idx int) string {
	if len(call.Arguments) <= idx {
		return ""
	}
	return call.Arguments[idx].String()
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return path
}

func resolveSymlinks(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
