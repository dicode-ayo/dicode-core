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

// ApplyOverrides is the exported entry point used by per-edge override
// dispatch sites (pkg/trigger preflight + chain firing). It returns a
// deep copy of base with the supplied layers merged on top, reusing the
// exact same logic that powers `dicode tasks override <id>` and the
// taskset resolver's three-level cascade.
//
// Callers MUST treat the input base as read-only; the returned spec is a
// fresh deep copy so mutating it does not affect the registry's canonical
// spec.
func ApplyOverrides(base *task.Spec, layers ...*Overrides) *task.Spec {
	return applyOverrides(base, layers...)
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
	if len(o.Fs) > 0 {
		spec.Permissions.FS = o.Fs
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
	// Switching to a non-webhook trigger must also drop WebhookAuth —
	// otherwise a stale `auth: true` would resurface if the trigger is
	// later switched back to a webhook.
	if p.Cron != nil {
		t.Cron = *p.Cron
		t.Webhook = ""
		t.WebhookAuth = false
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
		t.WebhookAuth = false
		t.Chain = nil
		t.Daemon = false
	}
	if p.Chain != nil {
		t.Chain = p.Chain
		t.Cron = ""
		t.Webhook = ""
		t.WebhookAuth = false
		t.Manual = false
		t.Daemon = false
	}
	if p.Daemon != nil {
		t.Daemon = *p.Daemon
		t.Cron = ""
		t.Webhook = ""
		t.WebhookAuth = false
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
// Tasks is merged via union (preserving order, deduping) so an override can append
// entries without clobbering the base list.
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
		// UNION (was full-replace) — append unique entries from overlay.
		existing := make(map[string]struct{}, len(out.Tasks))
		for _, t := range out.Tasks {
			existing[t] = struct{}{}
		}
		for _, t := range overlay.Tasks {
			if _, ok := existing[t]; !ok {
				out.Tasks = append(out.Tasks, t)
				existing[t] = struct{}{}
			}
		}
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
	if overlay.RunsListExpired {
		out.RunsListExpired = true
	}
	if overlay.RunsDeleteInput {
		out.RunsDeleteInput = true
	}
	if overlay.RunsPinInput {
		out.RunsPinInput = true
	}
	if overlay.RunsUnpinInput {
		out.RunsUnpinInput = true
	}
	if overlay.RunsReplay {
		out.RunsReplay = true
	}
	if overlay.RunsGetInput {
		out.RunsGetInput = true
	}
	if overlay.TasksTest {
		out.TasksTest = true
	}
	if overlay.SourcesSetDevMode {
		out.SourcesSetDevMode = true
	}
	if overlay.GitCommitPush {
		out.GitCommitPush = true
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
	// Trigger.Before is a slice of BeforeEntry; each BeforeEntry carries a
	// *task.Overrides pointer that per-firing dispatch may mutate. Without
	// this deep-clone two concurrent firings would alias the same Overrides
	// instance (survey §5.3).
	if s.Trigger.Before != nil {
		before := make([]task.BeforeEntry, len(s.Trigger.Before))
		for i, be := range s.Trigger.Before {
			before[i] = be
			if be.Overrides != nil {
				o := *be.Overrides
				before[i].Overrides = &o
			}
		}
		out.Trigger.Before = before
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
	// OnFailureChain pointer — deep-clone so a per-firing mutation can't
	// reach back into the registry's canonical spec (survey §5.3). The
	// inner Params map is also cloned because chain dispatch overlays
	// engine-stamped keys onto it.
	if s.OnFailureChain != nil {
		ofc := *s.OnFailureChain
		if s.OnFailureChain.Params != nil {
			ofc.Params = make(map[string]any, len(s.OnFailureChain.Params))
			for k, v := range s.OnFailureChain.Params {
				ofc.Params[k] = v
			}
		}
		out.OnFailureChain = &ofc
	}
	// RunInputs pointer — same deep-clone discipline as the other pointer
	// fields. The inner *bool fields are cloned so a per-firing override
	// flipping Enabled cannot reach back into the registry copy.
	if s.RunInputs != nil {
		ri := *s.RunInputs
		if s.RunInputs.Enabled != nil {
			b := *s.RunInputs.Enabled
			ri.Enabled = &b
		}
		if s.RunInputs.BodyFullTextual != nil {
			b := *s.RunInputs.BodyFullTextual
			ri.BodyFullTextual = &b
		}
		out.RunInputs = &ri
	}
	return &out
}
