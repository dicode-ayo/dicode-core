package taskset

import (
	"strings"

	"github.com/dicode/dicode/pkg/task"
)

// applyOverrides applies override layers to base in order (first = lowest priority).
// Nil layers are skipped. Returns a deep copy with all layers applied.
func applyOverrides(base *task.Spec, layers ...*Overrides) *task.Spec {
	out := copySpec(base)
	for _, o := range layers {
		if o == nil {
			continue
		}
		applyLayer(out, o)
	}
	return out
}

func applyLayer(spec *task.Spec, o *Overrides) {
	if o.Trigger != nil {
		applyTriggerPatch(&spec.Trigger, o.Trigger)
	}
	if len(o.Params) > 0 {
		mergeParams(&spec.Params, o.Params)
	}
	if len(o.Env) > 0 {
		spec.Env = mergeEnv(spec.Env, o.Env)
	}
	if o.Timeout != 0 {
		spec.Timeout = o.Timeout
	}
	if o.Runtime != "" {
		spec.Runtime = task.Runtime(o.Runtime)
	}
	if o.Notify != nil {
		spec.Notify = mergeNotify(spec.Notify, o.Notify)
	}
}

// applyTriggerPatch patches only the non-nil fields of p into t.
// Because a task may only have one trigger type, setting any trigger type
// clears the others (preserving the single-trigger invariant).
func applyTriggerPatch(t *task.TriggerConfig, p *TriggerPatch) {
	if p.Cron != nil {
		t.Cron = *p.Cron
		t.Webhook = ""
		t.Manual = false
		t.Chain = nil
		t.Daemon = false
	}
	if p.Webhook != nil {
		t.Webhook = *p.Webhook
		t.Cron = ""
		t.Manual = false
		t.Chain = nil
		t.Daemon = false
	}
	if p.Manual != nil {
		t.Manual = *p.Manual
		t.Cron = ""
		t.Webhook = ""
		t.Chain = nil
		t.Daemon = false
	}
	if p.Chain != nil {
		t.Chain = p.Chain
		t.Cron = ""
		t.Webhook = ""
		t.Manual = false
		t.Daemon = false
	}
	if p.Daemon != nil {
		t.Daemon = *p.Daemon
		t.Cron = ""
		t.Webhook = ""
		t.Manual = false
		t.Chain = nil
	}
	if p.Restart != nil {
		t.Restart = *p.Restart
	}
}

// mergeParams merges param overrides into the base list by name.
// If a param with the same name exists, its default (and optionally required) is patched.
// If no matching param exists, a new one is appended.
func mergeParams(params *[]task.Param, overrides []ParamOverride) {
	for _, po := range overrides {
		found := false
		for i := range *params {
			if (*params)[i].Name == po.Name {
				(*params)[i].Default = po.Default
				if po.Required != nil {
					(*params)[i].Required = *po.Required
				}
				found = true
				break
			}
		}
		if !found {
			p := task.Param{Name: po.Name, Default: po.Default}
			if po.Required != nil {
				p.Required = *po.Required
			}
			*params = append(*params, p)
		}
	}
}

// mergeEnv merges env entries by key (the part before '=').
// Overlay entries overwrite base entries with the same key; order is preserved.
func mergeEnv(base, overlay []string) []string {
	// Track key → value and insertion order.
	m := make(map[string]string, len(base)+len(overlay))
	order := make([]string, 0, len(base)+len(overlay))

	for _, e := range base {
		k := envKey(e)
		if _, seen := m[k]; !seen {
			order = append(order, k)
		}
		m[k] = e
	}
	for _, e := range overlay {
		k := envKey(e)
		if _, seen := m[k]; !seen {
			order = append(order, k)
		}
		m[k] = e
	}

	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, m[k])
	}
	return out
}

// envKey returns the key portion of a KEY=value entry, or the whole string if
// no '=' is present (bare variable name reference).
func envKey(e string) string {
	if i := strings.IndexByte(e, '='); i >= 0 {
		return e[:i]
	}
	return e
}

// defaultsToOverrides converts a Defaults block into an Overrides that can be
// slotted into the cascade. Only fields valid at the Defaults level are included.
func defaultsToOverrides(d *Defaults) *Overrides {
	if d == nil {
		return nil
	}
	return &Overrides{
		Timeout: d.Timeout,
		Retry:   d.Retry,
		Env:     d.Env,
		Trigger: d.Trigger,
		Notify:  d.Notify,
	}
}

// mergeNotify merges overlay on top of base; non-nil pointer fields in overlay win.
func mergeNotify(base, overlay *task.NotifyConfig) *task.NotifyConfig {
	if overlay == nil {
		return base
	}
	if base == nil {
		n := *overlay
		return &n
	}
	out := *base
	if overlay.OnSuccess != nil {
		out.OnSuccess = overlay.OnSuccess
	}
	if overlay.OnFailure != nil {
		out.OnFailure = overlay.OnFailure
	}
	return &out
}

// copySpec returns a deep copy of s so that override layers never mutate the
// original spec loaded from disk.
func copySpec(s *task.Spec) *task.Spec {
	if s == nil {
		return nil
	}
	out := *s

	if s.Params != nil {
		out.Params = make([]task.Param, len(s.Params))
		copy(out.Params, s.Params)
	}
	if s.Env != nil {
		out.Env = make([]string, len(s.Env))
		copy(out.Env, s.Env)
	}
	if s.FS != nil {
		out.FS = make([]task.FSEntry, len(s.FS))
		copy(out.FS, s.FS)
	}
	if s.Trigger.Chain != nil {
		chain := *s.Trigger.Chain
		out.Trigger.Chain = &chain
	}
	if s.Docker != nil {
		docker := *s.Docker
		out.Docker = &docker
	}
	if s.Notify != nil {
		n := *s.Notify
		out.Notify = &n
	}
	return &out
}
