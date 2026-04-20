package taskset

import (
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
	if o.Name != "" {
		spec.Name = o.Name
	}
	if o.Description != "" {
		spec.Description = o.Description
	}
	if o.Trigger != nil {
		applyTriggerPatch(&spec.Trigger, o.Trigger)
	}
	if len(o.Params) > 0 {
		mergeParams(&spec.Params, o.Params)
	}
	if len(o.Env) > 0 {
		spec.Permissions.Env = mergeEnvEntries(spec.Permissions.Env, o.Env)
	}
	if len(o.Net) > 0 {
		spec.Permissions.Net = o.Net
	}
	if o.Dicode != nil {
		spec.Permissions.Dicode = mergeDicodePerms(spec.Permissions.Dicode, o.Dicode)
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
	if p.Auth != nil {
		t.WebhookAuth = *p.Auth
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
func mergeParams(params *task.Params, overrides []ParamOverride) {
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

// mergeEnvEntries merges overlay entries into base by Name (overlay wins).
func mergeEnvEntries(base, overlay []task.EnvEntry) []task.EnvEntry {
	m := make(map[string]int, len(base))
	out := make([]task.EnvEntry, len(base))
	copy(out, base)
	for i, e := range out {
		m[e.Name] = i
	}
	for _, e := range overlay {
		if idx, exists := m[e.Name]; exists {
			out[idx] = e
		} else {
			m[e.Name] = len(out)
			out = append(out, e)
		}
	}
	return out
}

// mergeDicodePerms merges overlay on top of base, with overlay winning non-zero fields.
func mergeDicodePerms(base, overlay *task.DicodePermissions) *task.DicodePermissions {
	if overlay == nil {
		return base
	}
	if base == nil {
		c := *overlay
		return &c
	}
	out := *base
	if len(overlay.Tasks) > 0 {
		out.Tasks = overlay.Tasks
	}
	if len(overlay.MCP) > 0 {
		out.MCP = overlay.MCP
	}
	if overlay.ListTasks {
		out.ListTasks = true
	}
	if overlay.GetRuns {
		out.GetRuns = true
	}
	if overlay.SecretsWrite {
		out.SecretsWrite = true
	}
	return &out
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
	if s.Permissions.Env != nil {
		out.Permissions.Env = make([]task.EnvEntry, len(s.Permissions.Env))
		copy(out.Permissions.Env, s.Permissions.Env)
	}
	if s.Permissions.FS != nil {
		out.Permissions.FS = make([]task.FSEntry, len(s.Permissions.FS))
		copy(out.Permissions.FS, s.Permissions.FS)
	}
	if s.Permissions.Run != nil {
		out.Permissions.Run = make([]string, len(s.Permissions.Run))
		copy(out.Permissions.Run, s.Permissions.Run)
	}
	if s.Permissions.Net != nil {
		out.Permissions.Net = make([]string, len(s.Permissions.Net))
		copy(out.Permissions.Net, s.Permissions.Net)
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
	if s.Permissions.Dicode != nil {
		d := *s.Permissions.Dicode
		out.Permissions.Dicode = &d
	}
	return &out
}
