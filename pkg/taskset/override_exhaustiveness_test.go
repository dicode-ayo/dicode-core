package taskset

// Override-merge exhaustiveness guards (#388).
//
// The override merge surface is hand-maintained in several places:
//
//   - applyLayer / applyTriggerPatch / mergeDicodePerms / copySpec (override.go)
//   - mergeOverrides (resolver.go)
//
// Each site enumerates struct fields by hand, so adding a field to
// task.Overrides / task.TriggerPatch / task.Spec without touching the merge
// code silently drops the field — exactly how the DicodePermissions
// SecretsHas/Crypto drop happened (#383; that surface is already guarded by
// TestMergeDicodePerms_Exhaustive in override_test.go). The tests below use
// reflection to enumerate the real struct fields of the remaining surfaces,
// so any newly added field fails a test by name until it is either wired into
// the merge code or consciously added to an allowlist here.

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/task"
)

// populateValue sets v (which must be addressable) to a deterministic
// non-zero value, recursing into structs/pointers/slices/maps. depth caps
// recursion so self-referential types (Overrides.Entries → *Overrides,
// ChainTrigger.Overrides → *Overrides) terminate; at the cap, pointers are
// left non-nil but their pointees are only shallowly populated.
//
// Unsupported kinds fail the test: a new field of an exotic kind must be
// consciously handled here rather than silently skipped.
func populateValue(t *testing.T, v reflect.Value, depth int) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(7) // covers time.Duration too
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(7)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(7)
	case reflect.Interface:
		v.Set(reflect.ValueOf("x"))
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		if depth > 0 {
			populateValue(t, v.Elem(), depth-1)
		}
	case reflect.Slice:
		elem := reflect.New(v.Type().Elem()).Elem()
		if depth > 0 {
			populateValue(t, elem, depth-1)
		}
		v.Set(reflect.Append(reflect.MakeSlice(v.Type(), 0, 1), elem))
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		key := reflect.New(v.Type().Key()).Elem()
		val := reflect.New(v.Type().Elem()).Elem()
		if depth > 0 {
			populateValue(t, key, depth-1)
			populateValue(t, val, depth-1)
		}
		m.SetMapIndex(key, val)
		v.Set(m)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).PkgPath != "" {
				continue // unexported
			}
			if depth > 0 {
				populateValue(t, v.Field(i), depth-1)
			}
		}
	default:
		t.Fatalf("populateValue: unsupported kind %s (type %s) — a new field uses a kind this test cannot fill; extend populateValue", v.Kind(), v.Type())
	}
}

// checkAllFieldsNonZero asserts every exported field of v is non-zero, except
// fields named in allowlist. hint is appended to the failure message.
func checkAllFieldsNonZero(t *testing.T, v reflect.Value, typeName string, allowlist map[string]string, hint string) {
	t.Helper()
	for i := 0; i < v.NumField(); i++ {
		f := v.Type().Field(i)
		if f.PkgPath != "" {
			continue
		}
		if _, ok := allowlist[f.Name]; ok {
			continue
		}
		if v.Field(i).IsZero() {
			t.Errorf("field %s.%s added but %s", typeName, f.Name, hint)
		}
	}
}

// ── task.Overrides ⊕ applyLayer ──────────────────────────────────────────────
//
// applyLayer is the merge that turns an Overrides layer into task.Spec
// mutations. Every Overrides field must either have a propagation check below
// or an allowlist entry naming the code that consumes it instead.
func TestApplyLayer_Exhaustive(t *testing.T) {
	// Fields NOT applied by applyLayer, with the site that consumes them.
	allowlist := map[string]string{
		"Enabled":  "consumed by Resolver.resolveBody (entry enable/disable), not applyLayer",
		"Retry":    "not applied anywhere yet — rejected per-edge by validatePerEdgeOverrides; wire into applyLayer when per-task retry lands",
		"Defaults": "taskset-level cascade construct consumed by Resolver.resolveBody/buildOverrideLayers",
		"Entries":  "nested-taskset patching consumed by Resolver.resolveBody (mergeOverrides recursion)",
	}

	o := &Overrides{
		Name:        "new-name",
		Description: "new-desc",
		Trigger:     &TriggerPatch{Cron: strPtr("0 9 * * *")},
		Params:      []ParamOverride{{Name: "p1", Default: "v1"}},
		Env:         []task.EnvEntry{{Name: "E1", Value: "v"}},
		Net:         []string{"api.example.com"},
		Fs:          []task.FSEntry{{Path: "/tmp/x", Permission: "r"}},
		Timeout:     42 * time.Second,
		Runtime:     "python",
		Dicode:      &task.DicodePermissions{ListTasks: true},
	}

	base := &task.Spec{
		Name:    "orig",
		Runtime: task.RuntimeDeno,
		Trigger: task.TriggerConfig{Manual: true},
	}
	merged := applyOverrides(base, o)

	// One propagation check per handled field. Keyed by field name so the
	// reflection sweep below can prove the union of checks+allowlist covers
	// the struct exactly.
	checks := map[string]func() error{
		"Name": func() error {
			return expectEq(merged.Name, "new-name")
		},
		"Description": func() error {
			return expectEq(merged.Description, "new-desc")
		},
		"Trigger": func() error {
			return expectEq(merged.Trigger.Cron, "0 9 * * *")
		},
		"Params": func() error {
			for _, p := range merged.Params {
				if p.Name == "p1" && p.Default == "v1" {
					return nil
				}
			}
			return fmt.Errorf("param p1=v1 not merged into spec.Params: %+v", merged.Params)
		},
		"Env": func() error {
			for _, e := range merged.Permissions.Env {
				if e.Name == "E1" && e.Value == "v" {
					return nil
				}
			}
			return fmt.Errorf("env E1 not merged into spec.Permissions.Env: %+v", merged.Permissions.Env)
		},
		"Net": func() error {
			if len(merged.Permissions.Net) == 1 && merged.Permissions.Net[0] == "api.example.com" {
				return nil
			}
			return fmt.Errorf("net override not applied: %+v", merged.Permissions.Net)
		},
		"Fs": func() error {
			if len(merged.Permissions.FS) == 1 && merged.Permissions.FS[0].Path == "/tmp/x" {
				return nil
			}
			return fmt.Errorf("fs override not applied: %+v", merged.Permissions.FS)
		},
		"Timeout": func() error {
			return expectEq(merged.Timeout, 42*time.Second)
		},
		"Runtime": func() error {
			return expectEq(merged.Runtime, task.Runtime("python"))
		},
		"Dicode": func() error {
			if merged.Permissions.Dicode != nil && merged.Permissions.Dicode.ListTasks {
				return nil
			}
			return fmt.Errorf("dicode override not applied: %+v", merged.Permissions.Dicode)
		},
	}

	ot := reflect.TypeOf(Overrides{})
	for i := 0; i < ot.NumField(); i++ {
		name := ot.Field(i).Name
		_, hasCheck := checks[name]
		_, allowed := allowlist[name]
		switch {
		case hasCheck && allowed:
			t.Errorf("field task.Overrides.%s is both checked and allowlisted — pick one", name)
		case !hasCheck && !allowed:
			t.Errorf("field task.Overrides.%s added but not handled in applyLayer (pkg/taskset/override.go) — wire it into the merge and add a propagation check here, or add it to this test's allowlist naming the code that consumes it", name)
		}
	}
	for name, check := range checks {
		if err := check(); err != nil {
			t.Errorf("field task.Overrides.%s not propagated by applyLayer: %v", name, err)
		}
	}
}

// ── task.TriggerPatch ⊕ applyTriggerPatch ────────────────────────────────────
//
// applyTriggerPatch hand-copies every patch field (and clears siblings to
// keep the single-trigger invariant). A new TriggerPatch field without a case
// here fails the reflection sweep.
func TestApplyTriggerPatch_Exhaustive(t *testing.T) {
	cases := map[string]struct {
		patch TriggerPatch
		check func(tc task.TriggerConfig) error
	}{
		"Cron": {TriggerPatch{Cron: strPtr("0 9 * * *")}, func(tc task.TriggerConfig) error {
			return expectEq(tc.Cron, "0 9 * * *")
		}},
		"Webhook": {TriggerPatch{Webhook: strPtr("/hooks/x")}, func(tc task.TriggerConfig) error {
			return expectEq(tc.Webhook, "/hooks/x")
		}},
		"Auth": {TriggerPatch{Auth: boolPtr(true)}, func(tc task.TriggerConfig) error {
			return expectEq(tc.WebhookAuth, task.WebhookAuthSession)
		}},
		"Manual": {TriggerPatch{Manual: boolPtr(true)}, func(tc task.TriggerConfig) error {
			return expectEq(tc.Manual, true)
		}},
		"Chain": {TriggerPatch{Chain: &task.ChainTrigger{From: "up"}}, func(tc task.TriggerConfig) error {
			if tc.Chain == nil || tc.Chain.From != "up" {
				return fmt.Errorf("chain patch not applied: %+v", tc.Chain)
			}
			return nil
		}},
		"Daemon": {TriggerPatch{Daemon: boolPtr(true)}, func(tc task.TriggerConfig) error {
			return expectEq(tc.Daemon, true)
		}},
		"Restart": {TriggerPatch{Restart: strPtr("never")}, func(tc task.TriggerConfig) error {
			return expectEq(tc.Restart, "never")
		}},
	}

	pt := reflect.TypeOf(TriggerPatch{})
	for i := 0; i < pt.NumField(); i++ {
		name := pt.Field(i).Name
		if _, ok := cases[name]; !ok {
			t.Errorf("field task.TriggerPatch.%s added but not handled in applyTriggerPatch (pkg/taskset/override.go) — wire it into the patch and add a case here", name)
		}
	}
	for name, c := range cases {
		tc := task.TriggerConfig{Manual: true} // pre-existing trigger to overwrite
		applyTriggerPatch(&tc, &c.patch)
		if err := c.check(tc); err != nil {
			t.Errorf("field task.TriggerPatch.%s not propagated by applyTriggerPatch: %v", name, err)
		}
	}
}

// ── task.Overrides ⊕ mergeOverrides (resolver.go) ────────────────────────────
//
// mergeOverrides combines a parent entry patch (a) with an entry's own
// overrides (b), b winning and gaps filled from a. Any field it does not
// gap-fill is silently dropped from the parent patch when the entry override
// leaves it unset — the same field-drop failure class as the
// DicodePermissions bug. New fields must be gap-filled or allowlisted.
func TestMergeOverrides_Exhaustive(t *testing.T) {
	// Fields NOT gap-filled from a by mergeOverrides today. These are
	// pre-existing drops documented as-is when this guard was added (#388);
	// they are candidates for follow-up fixes, not deliberate design. If you
	// wire one of them into mergeOverrides, remove it here so the guard
	// protects it.
	allowlist := map[string]string{
		"Name":        "pre-existing drop: parent-patch name is lost when the entry override has none",
		"Description": "pre-existing drop: parent-patch description is lost when the entry override has none",
		"Net":         "pre-existing drop: parent-patch net is lost when the entry override has none",
		"Fs":          "pre-existing drop: parent-patch fs is lost when the entry override has none",
		"Dicode":      "pre-existing drop: parent-patch dicode perms are lost when the entry override has none",
	}

	a := &Overrides{}
	populateValue(t, reflect.ValueOf(a).Elem(), 3)
	got := mergeOverrides(a, &Overrides{})
	checkAllFieldsNonZero(t, reflect.ValueOf(got).Elem(), "task.Overrides", allowlist,
		"not gap-filled in mergeOverrides (pkg/taskset/resolver.go) — a parent entry patch setting it is silently dropped; wire it into the merge or add it to this test's allowlist")

	// b-wins direction: an empty parent patch must not clobber the entry's
	// own overrides.
	b := &Overrides{}
	populateValue(t, reflect.ValueOf(b).Elem(), 3)
	got = mergeOverrides(&Overrides{}, b)
	checkAllFieldsNonZero(t, reflect.ValueOf(got).Elem(), "task.Overrides", map[string]string{},
		"dropped from b by mergeOverrides (pkg/taskset/resolver.go) even though b wins — update the merge and this test's allowlist")
}

// ── task.Spec ⊕ copySpec ─────────────────────────────────────────────────────
//
// copySpec must deep-clone every pointer/slice/map field so an override layer
// (or per-firing mutation) can never reach back into the registry's canonical
// spec. It hand-enumerates the fields, so a NEW pointer/slice/map field on
// task.Spec (or its inline Trigger/Permissions structs) aliases the original
// until copySpec is updated. This sweep detects aliasing generically.
func TestCopySpec_Exhaustive(t *testing.T) {
	// Fields copySpec deliberately shares (aliases) between original and
	// copy. Safe only while nothing mutates them after load. If merge or
	// dispatch code starts writing through any of these, deep-clone it in
	// copySpec and remove it here.
	allowlist := map[string]string{
		"Warnings":                 "load-time diagnostics; append-only during validate(), never mutated post-copy",
		"Trigger.ReplayProtection": "*bool read-only after load; no merge path writes through it",
		"Trigger.RequireTimestamp": "*bool read-only after load; no merge path writes through it",
		"Provider":                 "provider config read-only after load; no merge path writes through it",
		"RunResult":                "run-result config read-only after load; no merge path writes through it",
		"AutoFix":                  "auto-fix config read-only after load; no merge path writes through it",
	}

	orig := &task.Spec{}
	populateValue(t, reflect.ValueOf(orig).Elem(), 4)
	cp := copySpec(orig)

	checkNoAliasing(t, reflect.ValueOf(orig).Elem(), reflect.ValueOf(cp).Elem(), "", allowlist)
}

// checkNoAliasing walks the exported fields of orig/cp in lockstep. For
// pointer/slice/map fields it asserts the copy does not share memory with the
// original; for inline struct fields it recurses (so Trigger.* and
// Permissions.* are covered like copySpec covers them).
func checkNoAliasing(t *testing.T, orig, cp reflect.Value, prefix string, allowlist map[string]string) {
	t.Helper()
	for i := 0; i < orig.NumField(); i++ {
		f := orig.Type().Field(i)
		if f.PkgPath != "" {
			continue
		}
		name := f.Name
		if prefix != "" {
			name = prefix + "." + f.Name
		}
		if _, ok := allowlist[name]; ok {
			continue
		}
		of, cf := orig.Field(i), cp.Field(i)
		switch of.Kind() {
		case reflect.Pointer, reflect.Map:
			if !of.IsNil() && of.Pointer() == cf.Pointer() {
				t.Errorf("field task.Spec.%s added but not deep-cloned in copySpec (pkg/taskset/override.go) — the copy aliases the original; clone it in copySpec or add it to this test's allowlist with a why-it-is-safe note", name)
			}
		case reflect.Slice:
			if of.Len() > 0 && of.Pointer() == cf.Pointer() {
				t.Errorf("field task.Spec.%s added but not deep-cloned in copySpec (pkg/taskset/override.go) — the copy shares the original's backing array; clone it in copySpec or add it to this test's allowlist with a why-it-is-safe note", name)
			}
		case reflect.Struct:
			checkNoAliasing(t, of, cf, name, allowlist)
		}
	}
}

// strPtr / boolPtr live in override_test.go (same package).

func expectEq[T comparable](got, want T) error {
	if got != want {
		return fmt.Errorf("got %v, want %v", got, want)
	}
	return nil
}
