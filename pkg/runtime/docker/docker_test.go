package docker

import (
	"testing"

	"github.com/dicode/dicode/pkg/runtime/envresolve"
	"github.com/dicode/dicode/pkg/secrets"
)

// redactorFor mirrors the derivation Run performs from PreResolvedEnv: the
// run-log redactor is built from the resolved secret values, or nil when
// there is no PreResolvedEnv.
func redactorFor(pre *envresolve.Resolved) *secrets.Redactor {
	if pre == nil {
		return nil
	}
	return secrets.NewRedactor(pre.Secrets)
}

// A container log line that echoes a resolved secret value must be redacted
// before it reaches the run log — this is the wiring streamLines relies on.
func TestStreamLineRedaction_RedactsSecretValue(t *testing.T) {
	pre := &envresolve.Resolved{
		Env:     map[string]string{"API_TOKEN": "s3cr3t-value"},
		Secrets: map[string]string{"API_TOKEN": "s3cr3t-value"},
	}
	r := redactorFor(pre)
	got := r.RedactString("connecting with token s3cr3t-value ok")
	if got != "connecting with token "+secrets.RedactionMarker+" ok" {
		t.Fatalf("secret not redacted: %q", got)
	}
}

// With no PreResolvedEnv the redactor is nil, and RedactString on a nil
// receiver is a passthrough — logs stream unchanged (no regression).
func TestStreamLineRedaction_NilRedactorPassthrough(t *testing.T) {
	r := redactorFor(nil)
	line := "no secrets here"
	if got := r.RedactString(line); got != line {
		t.Fatalf("nil redactor mutated line: %q", got)
	}
}
