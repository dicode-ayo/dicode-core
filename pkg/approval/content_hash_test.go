package approval

import (
	"testing"
	"time"

	"github.com/dicode/dicode/pkg/task"
)

// specVariant returns a dir-backed spec for the given task dir (created once
// by the caller via writeTaskDir) with the supplied mutation applied, so tests
// can vary single resolved fields against an identical on-disk dir.
func specVariant(base *task.Spec, mutate func(*task.Spec)) *task.Spec {
	s := &task.Spec{ID: base.ID, TaskDir: base.TaskDir}
	if mutate != nil {
		mutate(s)
	}
	return s
}

func mustHash(t *testing.T, k task.Kinded) string {
	t.Helper()
	h, err := ContentHash(k)
	if err != nil {
		t.Fatalf("ContentHash: %v", err)
	}
	if h == "" {
		t.Fatal("ContentHash returned empty hash")
	}
	return h
}

func TestContentHashStableAcrossCalls(t *testing.T) {
	base := writeTaskDir(t, t.TempDir(), "repo/deploy", "x")
	spec := specVariant(base, func(s *task.Spec) {
		s.Runtime = task.RuntimeDeno
		s.Permissions.Net = []string{"api.github.com"}
		s.Permissions.Dicode = &task.DicodePermissions{Tasks: []string{"repo/other"}}
		s.Trigger.WebhookAuth = task.WebhookAuthSession
	})
	h1 := mustHash(t, spec)
	h2 := mustHash(t, spec)
	if h1 != h2 {
		t.Fatalf("ContentHash not stable: %q vs %q", h1, h2)
	}
}

// TestContentHashFoldsResolvedSecurityFields is the core of issue #400:
// taskset overrides mutate the resolved spec outside the task dir, so any
// security-bearing resolved field must perturb the hash even when the dir is
// byte-identical.
func TestContentHashFoldsResolvedSecurityFields(t *testing.T) {
	base := writeTaskDir(t, t.TempDir(), "repo/deploy", "x")

	variants := map[string]func(*task.Spec){
		"net wildcard":     func(s *task.Spec) { s.Permissions.Net = []string{"*"} },
		"env read exposed": func(s *task.Spec) { s.Permissions.EnvReadExposed = true },
		"fs write grant":   func(s *task.Spec) { s.Permissions.FS = []task.FSEntry{{Path: "/etc", Permission: "w"}} },
		"dicode tasks":     func(s *task.Spec) { s.Permissions.Dicode = &task.DicodePermissions{Tasks: []string{"*"}} },
		"runtime swap":     func(s *task.Spec) { s.Runtime = task.Runtime("python") },
		"webhook auth on":  func(s *task.Spec) { s.Trigger.WebhookAuth = task.WebhookAuthSession },
		"param default":    func(s *task.Spec) { s.Params = task.Params{{Name: "url", Default: "https://evil.example"}} },
		"timeout widened":  func(s *task.Spec) { s.Timeout = 4 * time.Hour },
	}

	baseHash := mustHash(t, specVariant(base, nil))
	for name, mutate := range variants {
		got := mustHash(t, specVariant(base, mutate))
		if got == baseHash {
			t.Errorf("%s: hash unchanged despite elevated resolved field", name)
		}
	}
}

// TestContentHashDistinguishesAuthModes: session and any are distinct security
// postures (any opens a relay-reachable HMAC path), so they must hash
// differently — switching to any re-pends the task for approval. none and
// session keep the pre-tri-value bool wire format (see WebhookAuthMode.
// MarshalJSON), so an existing auth: true task does not spuriously re-pend.
func TestContentHashDistinguishesAuthModes(t *testing.T) {
	base := writeTaskDir(t, t.TempDir(), "repo/deploy", "x")
	withSecret := func(m task.WebhookAuthMode) func(*task.Spec) {
		return func(s *task.Spec) {
			s.Trigger.Webhook = "/hooks/x"
			s.Trigger.WebhookSecret = "s3cr3t"
			s.Trigger.WebhookAuth = m
		}
	}
	none := mustHash(t, specVariant(base, withSecret(task.WebhookAuthNone)))
	session := mustHash(t, specVariant(base, withSecret(task.WebhookAuthSession)))
	anyMode := mustHash(t, specVariant(base, withSecret(task.WebhookAuthAny)))

	if session == anyMode {
		t.Error("session and any must hash differently (any opens an HMAC-over-relay path)")
	}
	if none == session {
		t.Error("none and session must hash differently")
	}
	if none == anyMode {
		t.Error("none and any must hash differently")
	}
}

// TestContentHashFoldsParamDefault: param defaults are override-mutable
// program inputs (mergeParams); repointing one against an identical dir must
// perturb the hash, while a cosmetic param description edit must not.
func TestContentHashFoldsParamDefault(t *testing.T) {
	base := writeTaskDir(t, t.TempDir(), "repo/deploy", "x")

	a := mustHash(t, specVariant(base, func(s *task.Spec) {
		s.Params = task.Params{{Name: "url", Default: "https://api.example.com"}}
	}))
	b := mustHash(t, specVariant(base, func(s *task.Spec) {
		s.Params = task.Params{{Name: "url", Default: "https://exfil.example.com"}}
	}))
	if a == b {
		t.Fatal("param default change must perturb the hash (override-mutable program input)")
	}

	withDesc := mustHash(t, specVariant(base, func(s *task.Spec) {
		s.Params = task.Params{{Name: "url", Default: "https://api.example.com", Description: "the endpoint"}}
	}))
	if a != withDesc {
		t.Fatal("param description edit must not churn the hash")
	}
}

// TestContentHashFoldsTimeout: an override-widened timeout extends the
// wall-clock budget and must re-pend.
func TestContentHashFoldsTimeout(t *testing.T) {
	base := writeTaskDir(t, t.TempDir(), "repo/deploy", "x")
	a := mustHash(t, specVariant(base, func(s *task.Spec) { s.Timeout = time.Minute }))
	b := mustHash(t, specVariant(base, func(s *task.Spec) { s.Timeout = 24 * time.Hour }))
	if a == b {
		t.Fatal("timeout change must perturb the hash")
	}
}

// TestContentHashRedactsEnvLiteralValues: dicode.lock is committable, so a
// literal env value must never feed the hash (offline dictionary attack on a
// low-entropy credential). A Value-only change therefore keeps the hash; a
// NAME change (a different variable becomes injectable) must perturb it.
func TestContentHashRedactsEnvLiteralValues(t *testing.T) {
	base := writeTaskDir(t, t.TempDir(), "repo/deploy", "x")

	valA := mustHash(t, specVariant(base, func(s *task.Spec) {
		s.Permissions.Env = []task.EnvEntry{{Name: "API_TOKEN", Value: "hunter2"}}
	}))
	valB := mustHash(t, specVariant(base, func(s *task.Spec) {
		s.Permissions.Env = []task.EnvEntry{{Name: "API_TOKEN", Value: "correct-horse-battery-staple"}}
	}))
	if valA != valB {
		t.Fatal("env literal Value must be redacted from the hash input; value-only change churned the hash")
	}

	renamed := mustHash(t, specVariant(base, func(s *task.Spec) {
		s.Permissions.Env = []task.EnvEntry{{Name: "OTHER_TOKEN", Value: "hunter2"}}
	}))
	if renamed == valA {
		t.Fatal("env entry NAME change must perturb the hash")
	}
}

// TestContentHashDirlessFallbackSanitized: the dir-less *task.Spec fallback
// marshals the spec, and TriggerConfig has yaml tags only — every exported
// field would marshal by Go name. The fallback must clear WebhookSecret (and
// redact env literals) so the committable lock never embeds a digest over
// secret material.
func TestContentHashDirlessFallbackSanitized(t *testing.T) {
	mk := func(secret string) *task.Spec {
		return &task.Spec{
			ID: "set/inline",
			Trigger: task.TriggerConfig{
				Webhook:       "/hooks/inline",
				WebhookSecret: secret,
			},
		}
	}
	a := mustHash(t, mk("s3cret-one"))
	b := mustHash(t, mk("s3cret-two"))
	if a != b {
		t.Fatal("dir-less fallback must exclude Trigger.WebhookSecret from the hash input")
	}

	// Env literal redaction applies to the fallback too.
	mkEnv := func(val string) *task.Spec {
		s := mk("")
		s.Permissions.Env = []task.EnvEntry{{Name: "API_TOKEN", Value: val}}
		return s
	}
	if mustHash(t, mkEnv("one")) != mustHash(t, mkEnv("two")) {
		t.Fatal("dir-less fallback must redact env literal values")
	}

	// Sanity: the webhook path itself still folds.
	c := mk("")
	c.Trigger.Webhook = "/hooks/other"
	if mustHash(t, c) == a {
		t.Fatal("dir-less fallback lost non-secret trigger fields")
	}
}

// TestContentHashFoldsResolvedTriggerShape: a TriggerPatch override can
// switch a manual/cron task to an (unauthenticated) webhook, change its path,
// or rewire chain/daemon — all without touching the task dir or webhook_auth.
// Every such trigger-shape change must perturb the hash.
func TestContentHashFoldsResolvedTriggerShape(t *testing.T) {
	base := writeTaskDir(t, t.TempDir(), "repo/deploy", "x")

	pairs := []struct {
		name string
		a, b func(*task.Spec)
	}{
		{
			name: "manual to unauthenticated webhook",
			a:    func(s *task.Spec) { s.Trigger.Manual = true },
			b:    func(s *task.Spec) { s.Trigger.Webhook = "/hooks/x" },
		},
		{
			name: "webhook path change",
			a:    func(s *task.Spec) { s.Trigger.Webhook = "/hooks/a" },
			b:    func(s *task.Spec) { s.Trigger.Webhook = "/hooks/b" },
		},
		{
			name: "chain rewire",
			a:    nil,
			b:    func(s *task.Spec) { s.Trigger.Chain = &task.ChainTrigger{From: "repo/other"} },
		},
	}
	for _, p := range pairs {
		ha := mustHash(t, specVariant(base, p.a))
		hb := mustHash(t, specVariant(base, p.b))
		if ha == hb {
			t.Errorf("%s: hash unchanged despite trigger shape change", p.name)
		}
	}
}

// TestContentHashIgnoresCosmeticResolvedFields documents the intended scope:
// only security-bearing resolved fields are folded in; cosmetic resolved
// drift (e.g. an override-free description tweak applied post-load) must not
// churn approvals for dir-backed tasks.
func TestContentHashIgnoresCosmeticResolvedFields(t *testing.T) {
	base := writeTaskDir(t, t.TempDir(), "repo/deploy", "x")
	plain := mustHash(t, specVariant(base, nil))
	cosmetic := mustHash(t, specVariant(base, func(s *task.Spec) {
		s.Description = "totally different description"
		s.Name = "renamed"
	}))
	if plain != cosmetic {
		t.Fatalf("cosmetic field change altered hash: %q vs %q", plain, cosmetic)
	}
}

// TestContentHashPipelineFoldsResolvedTrigger: dir-backed pipelines get the
// same dirHash+resolved-fields scheme as Specs, so a future resolver that
// applies override layers to pipelines fails closed (re-pends) instead of
// silently keeping a dir-only approval. A manual→webhook rewire against an
// identical dir must perturb the hash, and the hash must not degrade to the
// plain dir hash.
func TestContentHashPipelineFoldsResolvedTrigger(t *testing.T) {
	// Reuse writeTaskDir for the directory contents; only the dir matters.
	dir := writeTaskDir(t, t.TempDir(), "repo/pipe", "stages").TaskDir

	manual := &task.PipelineTask{ID: "repo/pipe", TaskDir: dir,
		Trigger: task.PipelineTrigger{Manual: true}}
	webhook := &task.PipelineTask{ID: "repo/pipe", TaskDir: dir,
		Trigger: task.PipelineTrigger{Webhook: "/hooks/pipe"}}

	hm := mustHash(t, manual)
	hw := mustHash(t, webhook)
	if hm == hw {
		t.Fatal("pipeline trigger rewire (manual→webhook) must perturb the hash")
	}

	dirHash, err := task.Hash(dir)
	if err != nil {
		t.Fatalf("task.Hash: %v", err)
	}
	if hm == dirHash || hw == dirHash {
		t.Fatal("pipeline ContentHash must not be the plain dir hash")
	}
}

// TestOverrideElevationRePend is the gate-level regression test for issue
// #400: an approved task whose resolved spec is later elevated by a taskset
// override (same dir on disk) must be held pending again, not auto-armed off
// the stale lock entry — whether the elevation is a permission grant or a
// trigger rewire.
func TestOverrideElevationRePend(t *testing.T) {
	cases := []struct {
		name     string
		initial  func(*task.Spec)
		elevated func(*task.Spec)
	}{
		{
			// Override-elevated permissions (simulating a taskset.yaml edit
			// outside the task dir).
			name:    "elevated permissions",
			initial: nil,
			elevated: func(s *task.Spec) {
				s.Permissions.Net = []string{"*"}
				s.Permissions.Dicode = &task.DicodePermissions{SecretsWrite: true}
			},
		},
		{
			// A TriggerPatch override rewires the approved manual task to an
			// unauthenticated webhook (Webhook set, Manual cleared, auth false).
			name:     "trigger rewire to unauthenticated webhook",
			initial:  func(s *task.Spec) { s.Trigger.Manual = true },
			elevated: func(s *task.Spec) { s.Trigger.Webhook = "/hooks/x" },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, arm, lock := newTestGate(t, enabledPolicy())
			base := writeTaskDir(t, t.TempDir(), "repo/deploy", "v1")

			// Admit at the initial shape and approve.
			if armed, err := g.Admit(specVariant(base, tc.initial)); err != nil || armed {
				t.Fatalf("Admit initial = (%v, %v), want pending", armed, err)
			}
			if err := g.Approve("repo/deploy"); err != nil {
				t.Fatalf("Approve: %v", err)
			}
			approvedRec, _ := lock.Get("repo/deploy")

			// Same dir, but the resolved spec is now override-elevated.
			armed, err := g.Admit(specVariant(base, tc.elevated))
			if err != nil {
				t.Fatalf("Admit elevated: %v", err)
			}
			if armed {
				t.Fatal("override-elevated task must re-pend, got armed (issue #400 bypass)")
			}
			if !g.IsPending("repo/deploy") {
				t.Fatal("elevated task not in pending set")
			}
			if got := arm.armedIDs(); len(got) != 1 {
				t.Fatalf("armed = %v, want only the original approval", got)
			}
			// The lock keeps the previously approved hash for drift inspection.
			if rec, ok := lock.Get("repo/deploy"); !ok || rec != approvedRec {
				t.Fatalf("lock record changed on re-pend: %+v → %+v (ok=%v)", approvedRec, rec, ok)
			}
		})
	}
}
