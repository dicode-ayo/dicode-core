package task

import (
	"os"
	"testing"
)

func TestExpandString_BuiltinVar(t *testing.T) {
	vars := map[string]string{VarTaskDir: "/tmp/mytask"}
	got := expandString("${TASK_DIR}/../../skills", vars)
	want := "/tmp/mytask/../../skills"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExpandString_ProcessEnvFallback(t *testing.T) {
	t.Setenv("FOO_TEST_VAR", "bar")
	got := expandString("${FOO_TEST_VAR}/baz", map[string]string{})
	if got != "bar/baz" {
		t.Errorf("got %q, want %q", got, "bar/baz")
	}
}

func TestExpandString_UnknownVarIsPreserved(t *testing.T) {
	// Unknown vars must NOT collapse to empty string. Keeping the literal
	// makes bugs obvious and gives downstream code a chance to warn.
	os.Unsetenv("DICODE_TEMPLATE_UNKNOWN_TEST")
	got := expandString("${DICODE_TEMPLATE_UNKNOWN_TEST}/tail", map[string]string{})
	if got != "${DICODE_TEMPLATE_UNKNOWN_TEST}/tail" {
		t.Errorf("got %q, want the literal to be preserved", got)
	}
}

func TestExpandString_BuiltinBeatsProcessEnv(t *testing.T) {
	t.Setenv("TASK_DIR", "/shouldnt/win")
	vars := map[string]string{VarTaskDir: "/from/builtin"}
	got := expandString("${TASK_DIR}/x", vars)
	if got != "/from/builtin/x" {
		t.Errorf("got %q, want builtin to win", got)
	}
}

func TestExpandString_NoDollarSign_IsNoop(t *testing.T) {
	in := "/plain/path/with/no/template"
	if got := expandString(in, map[string]string{"X": "y"}); got != in {
		t.Errorf("got %q, want %q", got, in)
	}
}

func TestExpandString_Empty(t *testing.T) {
	if got := expandString("", map[string]string{"X": "y"}); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestExpandSpec_FSPaths(t *testing.T) {
	spec := &Spec{
		Permissions: Permissions{
			FS: []FSEntry{
				{Path: "${TASK_DIR}/../../skills", Permission: "r"},
				{Path: "/absolute/path", Permission: "w"},
				{Path: "relative/path", Permission: "rw"},
			},
		},
	}
	vars := map[string]string{VarTaskDir: "/repo/tasks/myagent"}
	expandSpec(spec, vars)

	if spec.Permissions.FS[0].Path != "/repo/tasks/myagent/../../skills" {
		t.Errorf("FS[0].Path = %q", spec.Permissions.FS[0].Path)
	}
	if spec.Permissions.FS[1].Path != "/absolute/path" {
		t.Errorf("FS[1].Path should be unchanged, got %q", spec.Permissions.FS[1].Path)
	}
	if spec.Permissions.FS[2].Path != "relative/path" {
		t.Errorf("FS[2].Path should be unchanged, got %q", spec.Permissions.FS[2].Path)
	}
}

func TestExpandSpec_WebhookSecret(t *testing.T) {
	t.Setenv("GITHUB_WEBHOOK_SECRET", "super-secret")
	spec := &Spec{
		Trigger: TriggerConfig{
			Webhook:       "/hooks/gh",
			WebhookSecret: "${GITHUB_WEBHOOK_SECRET}",
		},
	}
	expandSpec(spec, map[string]string{})

	if spec.Trigger.WebhookSecret != "super-secret" {
		t.Errorf("WebhookSecret = %q, want expansion to the env value", spec.Trigger.WebhookSecret)
	}
}

func TestExpandSpec_EnvEntryIndirection(t *testing.T) {
	t.Setenv("SECRETS_PREFIX", "prod")
	spec := &Spec{
		Permissions: Permissions{
			Env: []EnvEntry{
				{Name: "DB_PASS", Secret: "${SECRETS_PREFIX}_db_password"},
				{Name: "API_KEY", From: "${SECRETS_PREFIX}_api_token"},
			},
		},
	}
	expandSpec(spec, map[string]string{})

	if spec.Permissions.Env[0].Secret != "prod_db_password" {
		t.Errorf("Env[0].Secret = %q", spec.Permissions.Env[0].Secret)
	}
	if spec.Permissions.Env[1].From != "prod_api_token" {
		t.Errorf("Env[1].From = %q", spec.Permissions.Env[1].From)
	}
}

func TestBuiltinVars(t *testing.T) {
	vars := builtinVars("/repo/tasks/myagent")
	if vars[VarTaskDir] != "/repo/tasks/myagent" {
		t.Errorf("TASK_DIR not set correctly: %q", vars[VarTaskDir])
	}
	// HOME is best-effort — if UserHomeDir fails in CI, the map just won't
	// have it. We don't want to flake on that.
	if home, ok := vars[VarHome]; ok && home == "" {
		t.Errorf("HOME set but empty")
	}
}
