package globals

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dop251/goja"
	"github.com/dicode/dicode/pkg/db"
)

// InjectKV sets up the `kv` global backed by sqlite.
// Keys are namespaced per task ID to prevent collisions.
func InjectKV(vm *goja.Runtime, database db.DB, taskID string) {
	ns := taskID + ":"

	// Perform DB operations synchronously and resolve promise immediately.
	promise := func(fn func() (interface{}, error)) *goja.Promise {
		v, err := fn()
		p, resolve, reject := vm.NewPromise()
		if err != nil {
			_ = reject(err.Error())
		} else {
			_ = resolve(vm.ToValue(v))
		}
		return p
	}

	obj := vm.NewObject()

	_ = obj.Set("set", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) < 2 {
			panic(vm.ToValue("kv.set requires key and value"))
		}
		key := ns + call.Arguments[0].String()
		val, err := json.Marshal(call.Arguments[1].Export())
		if err != nil {
			panic(vm.ToValue(fmt.Sprintf("kv.set marshal: %v", err)))
		}
		return vm.ToValue(promise(func() (interface{}, error) {
			return nil, database.Exec(context.Background(),
				`INSERT INTO kv (key, value) VALUES (?, ?)
				 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
				key, string(val),
			)
		}))
	})

	_ = obj.Set("get", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			panic(vm.ToValue("kv.get requires key"))
		}
		key := ns + call.Arguments[0].String()
		return vm.ToValue(promise(func() (interface{}, error) {
			var raw string
			var found bool
			err := database.Query(context.Background(),
				`SELECT value FROM kv WHERE key = ?`, []any{key},
				func(rows db.Scanner) error {
					if rows.Next() {
						found = true
						return rows.Scan(&raw)
					}
					return nil
				},
			)
			if err != nil || !found {
				return nil, err
			}
			var out interface{}
			if err := json.Unmarshal([]byte(raw), &out); err != nil {
				return raw, nil
			}
			return out, nil
		}))
	})

	_ = obj.Set("delete", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			panic(vm.ToValue("kv.delete requires key"))
		}
		key := ns + call.Arguments[0].String()
		return vm.ToValue(promise(func() (interface{}, error) {
			return nil, database.Exec(context.Background(),
				`DELETE FROM kv WHERE key = ?`, key,
			)
		}))
	})

	_ = obj.Set("list", func(call goja.FunctionCall) goja.Value {
		prefix := ns
		if len(call.Arguments) > 0 {
			prefix = ns + call.Arguments[0].String()
		}
		return vm.ToValue(promise(func() (interface{}, error) {
			var keys []string
			err := database.Query(context.Background(),
				`SELECT key FROM kv WHERE key LIKE ? ORDER BY key`,
				[]any{prefix + "%"},
				func(rows db.Scanner) error {
					for rows.Next() {
						var k string
						if err := rows.Scan(&k); err != nil {
							return err
						}
						// Strip namespace prefix before returning to JS.
						keys = append(keys, k[len(ns):])
					}
					return nil
				},
			)
			return keys, err
		}))
	})

	vm.Set("kv", obj)
}
