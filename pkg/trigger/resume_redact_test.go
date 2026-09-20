package trigger

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/registry"
	"github.com/dicode/dicode/pkg/secrets"
	"github.com/dicode/dicode/pkg/task"
)

// TestSuspendRun_RedactsSecretBackedParamAtRest pins #817: a fire-time param
// whose value matches a live secrets-chain entry under its own name must not
// reach runs.resume_params in the clear — the same guarantee run inputs get
// from pkg/registry/inputredact.go. Before the fix, SuspendRun wrote
// opts.Params straight through with no redaction at all.
func TestSuspendRun_RedactsSecretBackedParamAtRest(t *testing.T) {
	exec := &suspendExec{}
	eng, reg := newSuspendEnv(t, exec)
	eng.SetSecrets(secrets.Chain{newMockSecrets(map[string]string{"api_key": "sk_live_topsecret"})})

	spec := &task.Spec{ID: "wiz", Name: "wiz", Runtime: task.RuntimeDeno,
		Params:      []task.Param{{Name: "api_key"}},
		Permissions: task.Permissions{Env: []task.EnvEntry{{Name: "API_KEY", Secret: "api_key"}}},
		Trigger:     task.TriggerConfig{Manual: true}, Enabled: true}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("register: %v", err)
	}

	origID, err := eng.FireManual(context.Background(), "wiz", map[string]string{"api_key": "sk_live_topsecret"})
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	orig := waitStatus(t, reg, origID, registry.StatusSuspended)

	if len(orig.ResumeParams) == 0 {
		t.Fatal("suspended run did not persist resume_params")
	}
	if strings.Contains(string(orig.ResumeParams), "sk_live_topsecret") {
		t.Errorf("resume_params blob at rest contains the raw secret value: %s", orig.ResumeParams)
	}
	if len(orig.ResumeParamsRedactedFields) != 1 || orig.ResumeParamsRedactedFields[0] != "params.api_key" {
		t.Errorf("ResumeParamsRedactedFields = %v, want [\"params.api_key\"]", orig.ResumeParamsRedactedFields)
	}

	var carry resumeCarry
	if err := json.Unmarshal(orig.ResumeParams, &carry); err != nil {
		t.Fatalf("decode resume_params: %v", err)
	}
	if carry.Params["api_key"] != registry.RedactPlaceholder {
		t.Errorf("stored api_key = %q, want the redaction placeholder", carry.Params["api_key"])
	}
}

// TestResumeRun_RestoresSecretBackedParam pins #817's other half: a param
// redacted at suspend time must come back with its REAL value on resume,
// re-resolved from the secrets chain — not the placeholder left in the blob.
// A resume that silently ran with "<redacted>" as a param value would be a
// worse regression than the disclosure #817 fixes.
func TestResumeRun_RestoresSecretBackedParam(t *testing.T) {
	exec := &suspendExec{}
	eng, reg := newSuspendEnv(t, exec)
	eng.SetSecrets(secrets.Chain{newMockSecrets(map[string]string{"api_key": "sk_live_topsecret"})})

	spec := &task.Spec{ID: "wiz", Name: "wiz", Runtime: task.RuntimeDeno,
		Params:      []task.Param{{Name: "api_key"}},
		Permissions: task.Permissions{Env: []task.EnvEntry{{Name: "API_KEY", Secret: "api_key"}}},
		Trigger:     task.TriggerConfig{Manual: true}, Enabled: true}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("register: %v", err)
	}

	origID, err := eng.FireManual(context.Background(), "wiz", map[string]string{"api_key": "sk_live_topsecret"})
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	orig := waitStatus(t, reg, origID, registry.StatusSuspended)

	newID, err := eng.ResumeRun(context.Background(), orig.ResumeToken, []byte(`{}`))
	if err != nil {
		t.Fatalf("ResumeRun: %v", err)
	}
	waitStatus(t, reg, newID, registry.StatusSuccess)

	exec.mu.Lock()
	defer exec.mu.Unlock()
	if got := exec.seenResumeParams["api_key"]; got != "sk_live_topsecret" {
		t.Errorf("continuation param api_key = %q, want the real secret value restored", got)
	}
}

// TestResumeRun_NonSecretParamRoundTripsUnchanged proves the redaction is
// scoped to values dicode can actually restore: a param whose value is NOT
// backed by a live secret under its own name — even one with a
// sensitive-looking name like "token" — must round-trip through
// resume_params byte-for-byte and must NOT appear in
// ResumeParamsRedactedFields. Destructively redacting it would lose the value
// forever (no secrets-chain fallback exists) and break the resumed run.
func TestResumeRun_NonSecretParamRoundTripsUnchanged(t *testing.T) {
	exec := &suspendExec{}
	eng, reg := newSuspendEnv(t, exec)
	eng.SetSecrets(secrets.Chain{newMockSecrets(nil)}) // configured chain, but no matching key

	spec := &task.Spec{ID: "wiz", Name: "wiz", Runtime: task.RuntimeDeno,
		Params: []task.Param{{Name: "project_name"}, {Name: "token"}},
		// A literal, non-secrets-store-backed value under a sensitive name —
		// e.g. a manual caller pasting a one-off credential into a param.
		Trigger: task.TriggerConfig{Manual: true}, Enabled: true}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("register: %v", err)
	}

	override := map[string]string{"project_name": "acme", "token": "literal-not-in-secrets-store"}
	origID, err := eng.FireManual(context.Background(), "wiz", override)
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	orig := waitStatus(t, reg, origID, registry.StatusSuspended)

	if len(orig.ResumeParamsRedactedFields) != 0 {
		t.Errorf("ResumeParamsRedactedFields = %v, want none (no live secret backs either param)", orig.ResumeParamsRedactedFields)
	}
	var carry resumeCarry
	if err := json.Unmarshal(orig.ResumeParams, &carry); err != nil {
		t.Fatalf("decode resume_params: %v", err)
	}
	if carry.Params["project_name"] != "acme" || carry.Params["token"] != "literal-not-in-secrets-store" {
		t.Errorf("stored params = %v, want both values unchanged", carry.Params)
	}

	newID, err := eng.ResumeRun(context.Background(), orig.ResumeToken, []byte(`{}`))
	if err != nil {
		t.Fatalf("ResumeRun: %v", err)
	}
	waitStatus(t, reg, newID, registry.StatusSuccess)

	exec.mu.Lock()
	defer exec.mu.Unlock()
	if got := exec.seenResumeParams["project_name"]; got != "acme" {
		t.Errorf("continuation project_name = %q, want %q", got, "acme")
	}
	if got := exec.seenResumeParams["token"]; got != "literal-not-in-secrets-store" {
		t.Errorf("continuation token = %q, want the original literal value unchanged", got)
	}
}

// TestSuspendRun_UngrantedSecretNameNeverResolvedOrLeaked closes the #886
// review finding: a param whose value happens to equal a live secrets-chain
// entry under its own name must NOT be treated as secret-backed unless the
// task's own permissions.env declares that exact name as a Secret key. Here
// "wiz" declares no permissions.env at all, so params.api_key must round-trip
// as the literal fire-time value the caller already had — and the
// continuation must never receive a value freshly re-resolved from the
// secrets chain (proven by rotating the "live" secret between suspend and
// resume and asserting the continuation still sees the STALE fire-time
// value, not the rotated one).
func TestSuspendRun_UngrantedSecretNameNeverResolvedOrLeaked(t *testing.T) {
	exec := &suspendExec{}
	eng, reg := newSuspendEnv(t, exec)
	mock := newMockSecrets(map[string]string{"api_key": "sk_live_initial"})
	eng.SetSecrets(secrets.Chain{mock})

	spec := &task.Spec{ID: "wiz", Name: "wiz", Runtime: task.RuntimeDeno,
		Params: []task.Param{{Name: "api_key"}},
		// No Permissions.Env entry for "api_key" — the task never declared a
		// secret grant under that name.
		Trigger: task.TriggerConfig{Manual: true}, Enabled: true}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("register: %v", err)
	}

	origID, err := eng.FireManual(context.Background(), "wiz", map[string]string{"api_key": "sk_live_initial"})
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	orig := waitStatus(t, reg, origID, registry.StatusSuspended)

	if len(orig.ResumeParamsRedactedFields) != 0 {
		t.Errorf("ResumeParamsRedactedFields = %v, want none: task never granted params.api_key as a secret", orig.ResumeParamsRedactedFields)
	}
	var carry resumeCarry
	if err := json.Unmarshal(orig.ResumeParams, &carry); err != nil {
		t.Fatalf("decode resume_params: %v", err)
	}
	if carry.Params["api_key"] != "sk_live_initial" {
		t.Errorf("stored api_key = %q, want the unredacted literal fire-time value", carry.Params["api_key"])
	}

	// Rotate the "live" secret. If restore (or anything else) still resolved
	// params.api_key against the secrets chain, the continuation would see
	// this rotated value instead of what the run actually fired with.
	mock.set("api_key", "sk_live_rotated_after_suspend")

	newID, err := eng.ResumeRun(context.Background(), orig.ResumeToken, []byte(`{}`))
	if err != nil {
		t.Fatalf("ResumeRun: %v", err)
	}
	waitStatus(t, reg, newID, registry.StatusSuccess)

	exec.mu.Lock()
	defer exec.mu.Unlock()
	if got := exec.seenResumeParams["api_key"]; got != "sk_live_initial" {
		t.Errorf("continuation param api_key = %q, want the original fire-time value %q (must never be re-resolved from the secrets chain without a permissions.env grant)",
			got, "sk_live_initial")
	}
}

// TestResumeRun_RevokedPermissionFailsRestoreWithoutStrandingRun pins two
// fixes together: (1) restoring a redacted field whose name is no longer
// granted by the task's current permissions.env must fail loudly rather than
// silently skip it or hand back the placeholder, and (2) that failure must
// happen BEFORE the resume token is consumed, so the run is left resumable
// (still `suspended`, token intact) rather than stranded in a terminal
// `resumed` state with no continuation ever spawned.
func TestResumeRun_RevokedPermissionFailsRestoreWithoutStrandingRun(t *testing.T) {
	exec := &suspendExec{}
	eng, reg := newSuspendEnv(t, exec)
	eng.SetSecrets(secrets.Chain{newMockSecrets(map[string]string{"api_key": "sk_live_topsecret"})})

	grantedSpec := &task.Spec{ID: "wiz", Name: "wiz", Runtime: task.RuntimeDeno,
		Params:      []task.Param{{Name: "api_key"}},
		Permissions: task.Permissions{Env: []task.EnvEntry{{Name: "API_KEY", Secret: "api_key"}}},
		Trigger:     task.TriggerConfig{Manual: true}, Enabled: true}
	if err := reg.Register(grantedSpec); err != nil {
		t.Fatalf("register: %v", err)
	}

	origID, err := eng.FireManual(context.Background(), "wiz", map[string]string{"api_key": "sk_live_topsecret"})
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	orig := waitStatus(t, reg, origID, registry.StatusSuspended)
	if len(orig.ResumeParamsRedactedFields) != 1 || orig.ResumeParamsRedactedFields[0] != "params.api_key" {
		t.Fatalf("ResumeParamsRedactedFields = %v, want [\"params.api_key\"]", orig.ResumeParamsRedactedFields)
	}

	// Simulate the task being edited between suspend and resume: the new spec
	// keeps the same param but no longer grants it as a secret.
	editedSpec := &task.Spec{ID: "wiz", Name: "wiz", Runtime: task.RuntimeDeno,
		Params:  []task.Param{{Name: "api_key"}},
		Trigger: task.TriggerConfig{Manual: true}, Enabled: true}
	if err := reg.Register(editedSpec); err != nil {
		t.Fatalf("re-register: %v", err)
	}

	if _, err := eng.ResumeRun(context.Background(), orig.ResumeToken, []byte(`{}`)); err == nil {
		t.Fatal("ResumeRun: want error when a redacted param's permission grant was revoked, got nil")
	}

	after, gerr := reg.GetRun(context.Background(), origID)
	if gerr != nil {
		t.Fatalf("GetRun: %v", gerr)
	}
	if after.Status != registry.StatusSuspended {
		t.Errorf("run status after failed restore = %q, want %q (must not be stranded)", after.Status, registry.StatusSuspended)
	}
	if after.ResumeToken == "" {
		t.Error("resume token was cleared despite the restore failing before the token was consumed")
	}

	// Re-grant the permission; the SAME token must still resume successfully.
	if err := reg.Register(grantedSpec); err != nil {
		t.Fatalf("re-register granted spec: %v", err)
	}
	newID, err := eng.ResumeRun(context.Background(), orig.ResumeToken, []byte(`{}`))
	if err != nil {
		t.Fatalf("ResumeRun after re-grant: %v", err)
	}
	waitStatus(t, reg, newID, registry.StatusSuccess)
}

// TestApiGetRun_NeverExposesRawResumeParams is the registry-level half of the
// webui wire-format guarantee: GetRun still returns the byte-for-byte
// ResumeParams blob for ResumeRun's own use (it needs the real bytes to
// reconstruct ctx.params) — it is pkg/webui's apiGetRun, not the registry,
// that strips it before the value ever reaches an HTTP response. This test
// only pins that the registry itself keeps the metadata (redacted field
// names) queryable independently of the blob.
func TestGetRun_ExposesRedactedFieldMetadataAlongsideBlob(t *testing.T) {
	exec := &suspendExec{}
	eng, reg := newSuspendEnv(t, exec)
	eng.SetSecrets(secrets.Chain{newMockSecrets(map[string]string{"api_key": "sk_live_topsecret"})})
	spec := &task.Spec{ID: "wiz", Name: "wiz", Runtime: task.RuntimeDeno,
		Params:      []task.Param{{Name: "api_key"}},
		Permissions: task.Permissions{Env: []task.EnvEntry{{Name: "API_KEY", Secret: "api_key"}}},
		Trigger:     task.TriggerConfig{Manual: true}, Enabled: true}
	if err := reg.Register(spec); err != nil {
		t.Fatalf("register: %v", err)
	}

	origID, err := eng.FireManual(context.Background(), "wiz", map[string]string{"api_key": "sk_live_topsecret"})
	if err != nil {
		t.Fatalf("FireManual: %v", err)
	}
	orig := waitStatus(t, reg, origID, registry.StatusSuspended)

	fetched, err := reg.GetRun(context.Background(), orig.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if len(fetched.ResumeParamsRedactedFields) != 1 || fetched.ResumeParamsRedactedFields[0] != "params.api_key" {
		t.Errorf("GetRun ResumeParamsRedactedFields = %v, want [\"params.api_key\"]", fetched.ResumeParamsRedactedFields)
	}
}
