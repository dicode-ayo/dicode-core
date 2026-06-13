package approval

import (
	"reflect"
	"testing"

	"github.com/dicode/dicode/pkg/task"
)

// TestOverrideFieldsClassified is a reflection tripwire: every field of
// task.Overrides and task.TriggerPatch must be explicitly classified as
// either FOLDED (it perturbs the approval content hash via
// resolvedSecurityFields) or EXEMPT (with a recorded justification). A new
// override field that lands in neither set fails this test, so the override
// surface can never silently grow past the approval gate (issue #400).
func TestOverrideFieldsClassified(t *testing.T) {
	// folded: override fields whose effect on the resolved spec is folded
	// into resolvedSecurityFields (see gate.go). The value notes where the
	// effect lands.
	overridesFolded := map[string]string{
		"Net":     "permissions.net → resolvedSecurityFields.Permissions",
		"Fs":      "permissions.fs → resolvedSecurityFields.Permissions",
		"Dicode":  "permissions.dicode → resolvedSecurityFields.Permissions",
		"Env":     "permissions.env → resolvedSecurityFields.Permissions (literal values redacted)",
		"Runtime": "resolvedSecurityFields.Runtime",
		"Timeout": "resolvedSecurityFields.Timeout",
		"Params":  "resolvedSecurityFields.Params (name/default/required tuples)",
		"Trigger": "resolvedSecurityFields trigger fields (see TriggerPatch below)",
	}
	// exempt: override fields deliberately NOT folded, each with a one-line
	// justification.
	overridesExempt := map[string]string{
		"Name":        "cosmetic: display label only, no capability change",
		"Description": "cosmetic: display text only, no capability change",
		"Enabled":     "scheduling: enables/disables firing, capability when running unchanged",
		"Retry":       "re-execution within the already-approved permission envelope",
		"Defaults":    "structural cascade container: its effects land in concrete folded fields",
		"Entries":     "structural cascade container: its effects land in concrete folded fields",
	}
	checkClassified(t, reflect.TypeOf(task.Overrides{}), overridesFolded, overridesExempt)

	// Every TriggerPatch field rewires the task's exposure surface and is
	// folded via the resolved trigger shape.
	triggerPatchFolded := map[string]string{
		"Cron":    "resolvedSecurityFields.Cron",
		"Webhook": "resolvedSecurityFields.Webhook",
		"Auth":    "resolvedSecurityFields.WebhookAuth",
		"Manual":  "resolvedSecurityFields.Manual",
		"Chain":   "resolvedSecurityFields.Chain",
		"Daemon":  "resolvedSecurityFields.Daemon",
		"Restart": "resolvedSecurityFields.Restart",
	}
	checkClassified(t, reflect.TypeOf(task.TriggerPatch{}), triggerPatchFolded, map[string]string{})
}

// checkClassified walks typ's fields and requires each to appear in exactly
// one of folded/exempt; it also rejects stale classifications for fields that
// no longer exist (renames must update the lists).
func checkClassified(t *testing.T, typ reflect.Type, folded, exempt map[string]string) {
	t.Helper()
	fields := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		fields[name] = true
		_, isFolded := folded[name]
		_, isExempt := exempt[name]
		switch {
		case isFolded && isExempt:
			t.Errorf("%s.%s is classified as both folded and exempt; pick one", typ.Name(), name)
		case !isFolded && !isExempt:
			t.Errorf("new override field %s.%s must be classified: fold it into resolvedSecurityFields or add it to the exempt list with justification", typ.Name(), name)
		}
	}
	for name := range folded {
		if !fields[name] {
			t.Errorf("folded classification references nonexistent field %s.%s (renamed or removed? update the list)", typ.Name(), name)
		}
	}
	for name := range exempt {
		if !fields[name] {
			t.Errorf("exempt classification references nonexistent field %s.%s (renamed or removed? update the list)", typ.Name(), name)
		}
	}
}
