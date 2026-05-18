package task

import (
	"os"
	"testing"
)

func TestExpandString_BuiltinVar(t *testing.T) {
	vars := map[string]string{VarTaskDir: "/tmp/mytask"}
	got := expandString("${TASK_DIR}/../../skills", vars, true)
	want := "/tmp/mytask/../../skills"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExpandString_ProcessEnvFallback(t *testing.T) {
	t.Setenv("FOO_TEST_VAR", "bar")
	got := expandString("${FOO_TEST_VAR}/baz", map[string]string{}, true)
	if got != "bar/baz" {
		t.Errorf("got %q, want %q", got, "bar/baz")
	}
}

func TestExpandString_UnknownVarIsPreserved(t *testing.T) {
	// Unknown vars must NOT collapse to empty string. Keeping the literal
	// makes bugs obvious and gives downstream code a chance to warn.
	os.Unsetenv("DICODE_TEMPLATE_UNKNOWN_TEST")
	got := expandString("${DICODE_TEMPLATE_UNKNOWN_TEST}/tail", map[string]string{}, true)
	if got != "${DICODE_TEMPLATE_UNKNOWN_TEST}/tail" {
		t.Errorf("got %q, want the literal to be preserved", got)
	}
}

func TestExpandString_BuiltinBeatsProcessEnv(t *testing.T) {
	t.Setenv("TASK_DIR", "/shouldnt/win")
	vars := map[string]string{VarTaskDir: "/from/builtin"}
	got := expandString("${TASK_DIR}/x", vars, true)
	if got != "/from/builtin/x" {
		t.Errorf("got %q, want builtin to win", got)
	}
}

func TestExpandString_NoDollarSign_IsNoop(t *testing.T) {
	in := "/plain/path/with/no/template"
	if got := expandString(in, map[string]string{"X": "y"}, true); got != in {
		t.Errorf("got %q, want %q", got, in)
	}
}

func TestExpandString_Empty(t *testing.T) {
	if got := expandString("", map[string]string{"X": "y"}, true); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// When envFallback is false, process env lookups must NOT happen — unknown
// vars stay literal. Guards the exfiltration fix for EnvEntry.Value.
func TestExpandString_NoEnvFallback(t *testing.T) {
	t.Setenv("DICODE_TEMPLATE_SECRET_TEST", "should-not-leak")
	got := expandString("${DICODE_TEMPLATE_SECRET_TEST}/tail", map[string]string{}, false)
	if got != "${DICODE_TEMPLATE_SECRET_TEST}/tail" {
		t.Errorf("env value leaked despite envFallback=false: %q", got)
	}
	// Builtins still resolve even when env fallback is off.
	got = expandString("${X}/tail", map[string]string{"X": "ok"}, false)
	if got != "ok/tail" {
		t.Errorf("builtin not honoured: %q", got)
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

// SECURITY: EnvEntry.Value must NOT pull from process env under any
// circumstances. It is a literal value injected into the task sandbox, so
// env-fallback expansion here would let any task.yaml exfiltrate daemon
// environment variables by writing `value: "${SOME_SECRET}"`.
func TestExpandSpec_EnvEntryValue_NoEnvExfiltration(t *testing.T) {
	t.Setenv("DICODE_DAEMON_SECRET_TEST", "super-sensitive")
	spec := &Spec{
		Permissions: Permissions{
			Env: []EnvEntry{
				{Name: "LEAK", Value: "${DICODE_DAEMON_SECRET_TEST}"},
			},
		},
	}
	expandSpec(spec, map[string]string{})

	if spec.Permissions.Env[0].Value == "super-sensitive" {
		t.Fatalf("env value exfiltrated via EnvEntry.Value: %q", spec.Permissions.Env[0].Value)
	}
	// The literal ${…} should be preserved so operators notice the mistake.
	if spec.Permissions.Env[0].Value != "${DICODE_DAEMON_SECRET_TEST}" {
		t.Errorf("Value = %q, want literal preserved", spec.Permissions.Env[0].Value)
	}
}

// Builtins and extras DO apply to EnvEntry.Value — only the env fallback
// is suppressed. This keeps the taskset override use case working:
// `overrides.env: [{name: FOO, value: "${TASK_DIR}/marker"}]`.
func TestExpandSpec_EnvEntryValue_BuiltinsStillExpand(t *testing.T) {
	spec := &Spec{
		Permissions: Permissions{
			Env: []EnvEntry{
				{Name: "MARKER", Value: "${TASK_DIR}/marker"},
			},
		},
	}
	expandSpec(spec, map[string]string{VarTaskDir: "/repo/task"})

	if spec.Permissions.Env[0].Value != "/repo/task/marker" {
		t.Errorf("builtin did not expand: %q", spec.Permissions.Env[0].Value)
	}
}

// Builtins and extras apply to Param.Default — lets task.yaml surface
// loader-provided paths (${TASK_SET_DIR}, ${TASK_DIR}, …) as parameter
// defaults that task code then reads via params.get().
func TestExpandSpec_ParamDefault_BuiltinsExpand(t *testing.T) {
	spec := &Spec{
		Params: Params{
			{Name: "shared_dir", Default: "${TASK_SET_DIR}/shared"},
			{Name: "local_dir", Default: "${TASK_DIR}/local"},
		},
	}
	expandSpec(spec, map[string]string{
		VarTaskSetDir: "/repo/tasks",
		VarTaskDir:    "/repo/tasks/agent",
	})

	if spec.Params[0].Default != "/repo/tasks/shared" {
		t.Errorf("TASK_SET_DIR not expanded in param default: %q", spec.Params[0].Default)
	}
	if spec.Params[1].Default != "/repo/tasks/agent/local" {
		t.Errorf("TASK_DIR not expanded in param default: %q", spec.Params[1].Default)
	}
}

// Same exfiltration guard as EnvEntry.Value: Param.Default is readable from
// task code at runtime, so env fallback must not leak daemon secrets.
func TestExpandSpec_ParamDefault_NoEnvExfiltration(t *testing.T) {
	t.Setenv("DICODE_DAEMON_SECRET_TEST", "super-sensitive")
	spec := &Spec{
		Params: Params{
			{Name: "leak", Default: "${DICODE_DAEMON_SECRET_TEST}"},
		},
	}
	expandSpec(spec, map[string]string{})

	if spec.Params[0].Default == "super-sensitive" {
		t.Fatalf("daemon env exfiltrated via Param.Default: %q", spec.Params[0].Default)
	}
	if spec.Params[0].Default != "${DICODE_DAEMON_SECRET_TEST}" {
		t.Errorf("Default = %q, want literal preserved", spec.Params[0].Default)
	}
}

func TestBuiltinVars(t *testing.T) {
	vars := builtinVars("/repo/tasks/myagent", nil)
	if vars[VarTaskDir] != "/repo/tasks/myagent" {
		t.Errorf("TASK_DIR not set correctly: %q", vars[VarTaskDir])
	}
	// HOME is best-effort — if UserHomeDir fails in CI, the map just won't
	// have it. We don't want to flake on that.
	if home, ok := vars[VarHome]; ok && home == "" {
		t.Errorf("HOME set but empty")
	}
}

func TestBuiltinVars_TaskSetDirPassedThrough(t *testing.T) {
	extras := map[string]string{VarTaskSetDir: "/repo/tasks"}
	vars := builtinVars("/repo/tasks/myagent", extras)

	if vars[VarTaskSetDir] != "/repo/tasks" {
		t.Errorf("TASK_SET_DIR not passed through: %q", vars[VarTaskSetDir])
	}
	if vars[VarTaskDir] != "/repo/tasks/myagent" {
		t.Errorf("TASK_DIR wrong: %q", vars[VarTaskDir])
	}
}

func TestExpand_DATADIR(t *testing.T) {
	vars := map[string]string{
		"DATADIR": "/var/lib/dicode",
	}
	got := expandString("${DATADIR}/run-inputs", vars, false)
	if got != "/var/lib/dicode/run-inputs" {
		t.Errorf("got %q, want /var/lib/dicode/run-inputs", got)
	}
}

func TestVarDataDir_Constant(t *testing.T) {
	if VarDataDir != "DATADIR" {
		t.Errorf("VarDataDir = %q, want DATADIR", VarDataDir)
	}
}

func TestExpandSpec_DockerVolumes(t *testing.T) {
	spec := &Spec{
		Docker: &DockerConfig{
			Volumes: []string{
				"${DATADIR}/cloudflared:/etc/cloudflared:ro",
				"${TASK_DIR}/local:/work:rw",
				"${UNKNOWN}/leave-literal:/x",
			},
		},
	}
	vars := map[string]string{
		"DATADIR":  "/home/u/.dicode",
		"TASK_DIR": "/repo/tasks/cf",
	}
	expandSpec(spec, vars)
	want := []string{
		"/home/u/.dicode/cloudflared:/etc/cloudflared:ro",
		"/repo/tasks/cf/local:/work:rw",
		"${UNKNOWN}/leave-literal:/x",
	}
	for i, got := range spec.Docker.Volumes {
		if got != want[i] {
			t.Errorf("Volumes[%d] = %q, want %q", i, got, want[i])
		}
	}
}

func TestExpandSpec_DockerVolumes_NoEnvFallback(t *testing.T) {
	// envFallback must be false: a task.yaml from an untrusted source must not be
	// able to reference a daemon env var as a mount source.
	t.Setenv("DAEMON_SECRET_PATH", "/etc/dicode/secrets")
	spec := &Spec{
		Docker: &DockerConfig{Volumes: []string{"${DAEMON_SECRET_PATH}:/x"}},
	}
	expandSpec(spec, map[string]string{})
	if spec.Docker.Volumes[0] != "${DAEMON_SECRET_PATH}:/x" {
		t.Errorf("Volumes[0] = %q, want literal preserved (no env fallback)", spec.Docker.Volumes[0])
	}
}

// trigger.before[].overrides.{params,fs,env} must receive the same template
// expansion as their top-level twins. The relay-server daemon's preflight
// pipeline embeds ${DATADIR}/relay/relay.yaml in overrides.params.path and
// overrides.fs.path; without expansion these reach Deno literally and the
// daemon fails to boot.
func TestExpandSpec_BeforeOverrides_ParamsAndFsExpand(t *testing.T) {
	spec := &Spec{
		Trigger: TriggerConfig{
			Daemon: true,
			Before: []BeforeEntry{
				{
					Task: "buildin/write-local",
					Overrides: &Overrides{
						Params: ParamOverrides{
							{Name: "path", Default: "${DATADIR}/relay/relay.yaml"},
							{Name: "content", Default: "${input.output}"},
						},
						Fs: []FSEntry{
							{Path: "${DATADIR}/relay", Permission: "rw"},
						},
					},
				},
			},
		},
	}
	vars := map[string]string{VarDataDir: "/var/dicode"}
	expandSpec(spec, vars)

	got := spec.Trigger.Before[0].Overrides.Params[0].Default
	if got != "/var/dicode/relay/relay.yaml" {
		t.Errorf("Params[0].Default = %q; want %q", got, "/var/dicode/relay/relay.yaml")
	}
	// Unknown tokens (PR2's dispatch-time interpolator territory, e.g.
	// ${input.output}) must be LEFT LITERAL by expandSpec.
	if got := spec.Trigger.Before[0].Overrides.Params[1].Default; got != "${input.output}" {
		t.Errorf("Params[1].Default unknown-token mangled: %q; want literal preserved", got)
	}
	if got := spec.Trigger.Before[0].Overrides.Fs[0].Path; got != "/var/dicode/relay" {
		t.Errorf("Fs[0].Path = %q; want %q", got, "/var/dicode/relay")
	}
}

// Same exfiltration guard as the top-level Param.Default: overrides.params
// defaults are readable from the prereq task's code at runtime (the engine
// merges them into the param set the script sees), so envFallback must be
// false.
func TestExpandSpec_BeforeOverrides_ParamDefault_NoEnvExfiltration(t *testing.T) {
	t.Setenv("DICODE_DAEMON_SECRET_TEST", "super-sensitive")
	spec := &Spec{
		Trigger: TriggerConfig{
			Daemon: true,
			Before: []BeforeEntry{
				{
					Task: "buildin/write-local",
					Overrides: &Overrides{
						Params: ParamOverrides{
							{Name: "leak", Default: "${DICODE_DAEMON_SECRET_TEST}"},
						},
					},
				},
			},
		},
	}
	expandSpec(spec, map[string]string{})
	got := spec.Trigger.Before[0].Overrides.Params[0].Default
	if got == "super-sensitive" {
		t.Fatalf("daemon env exfiltrated via before.overrides.params[].default: %q", got)
	}
	if got != "${DICODE_DAEMON_SECRET_TEST}" {
		t.Errorf("Default = %q, want literal preserved", got)
	}
}

// EnvEntry inside before.overrides should follow the same per-field
// envFallback policy as top-level Permissions.Env (From/Secret may env-fall
// back, Value may not).
func TestExpandSpec_BeforeOverrides_EnvEntries(t *testing.T) {
	t.Setenv("SECRETS_PREFIX", "prod")
	t.Setenv("DICODE_DAEMON_SECRET_TEST", "super-sensitive")
	spec := &Spec{
		Trigger: TriggerConfig{
			Daemon: true,
			Before: []BeforeEntry{
				{
					Task: "buildin/x",
					Overrides: &Overrides{
						Env: []EnvEntry{
							{Name: "DB_PASS", Secret: "${SECRETS_PREFIX}_db"},
							{Name: "API_KEY", From: "${SECRETS_PREFIX}_token"},
							{Name: "LEAK", Value: "${DICODE_DAEMON_SECRET_TEST}"},
						},
					},
				},
			},
		},
	}
	expandSpec(spec, map[string]string{})
	env := spec.Trigger.Before[0].Overrides.Env
	if env[0].Secret != "prod_db" {
		t.Errorf("Env[0].Secret = %q", env[0].Secret)
	}
	if env[1].From != "prod_token" {
		t.Errorf("Env[1].From = %q", env[1].From)
	}
	if env[2].Value == "super-sensitive" {
		t.Fatalf("env exfiltrated via before.overrides.env[].value: %q", env[2].Value)
	}
	if env[2].Value != "${DICODE_DAEMON_SECRET_TEST}" {
		t.Errorf("Env[2].Value = %q, want literal preserved", env[2].Value)
	}
}

// chain.overrides.{params,fs,env} mirror before.overrides — chain edges and
// before edges are treated symmetrically by the per-edge dispatch path.
func TestExpandSpec_ChainOverrides_ParamsAndFsExpand(t *testing.T) {
	spec := &Spec{
		Trigger: TriggerConfig{
			Chain: &ChainTrigger{
				From: "upstream",
				Overrides: &Overrides{
					Params: ParamOverrides{
						{Name: "path", Default: "${DATADIR}/downstream/out.yaml"},
					},
					Fs: []FSEntry{
						{Path: "${DATADIR}/downstream", Permission: "rw"},
					},
				},
			},
		},
	}
	vars := map[string]string{VarDataDir: "/var/dicode"}
	expandSpec(spec, vars)

	if got := spec.Trigger.Chain.Overrides.Params[0].Default; got != "/var/dicode/downstream/out.yaml" {
		t.Errorf("chain Params[0].Default = %q", got)
	}
	if got := spec.Trigger.Chain.Overrides.Fs[0].Path; got != "/var/dicode/downstream" {
		t.Errorf("chain Fs[0].Path = %q", got)
	}
}

func TestLoadDirWithVars_ExpandsTaskSetDir(t *testing.T) {
	dir := t.TempDir()
	yaml := `
apiVersion: dicode/v1
kind: Task
name: test
runtime: deno
trigger:
  manual: true
permissions:
  fs:
    - path: "${TASK_SET_DIR}/skills"
      permission: r
    - path: "${TASK_DIR}/local"
      permission: r
`
	if err := os.WriteFile(dir+"/task.yaml", []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create task.ts to satisfy validation.
	if err := os.WriteFile(dir+"/task.ts", []byte("export default async function main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	spec, err := LoadDirWithVars(dir, map[string]string{VarTaskSetDir: "/fake/source"})
	if err != nil {
		t.Fatalf("LoadDirWithVars: %v", err)
	}

	if len(spec.Permissions.FS) != 2 {
		t.Fatalf("expected 2 FS entries, got %d", len(spec.Permissions.FS))
	}
	if spec.Permissions.FS[0].Path != "/fake/source/skills" {
		t.Errorf("FS[0] = %q, want /fake/source/skills", spec.Permissions.FS[0].Path)
	}
	if spec.Permissions.FS[1].Path != dir+"/local" {
		t.Errorf("FS[1] = %q, want %s/local", spec.Permissions.FS[1].Path, dir)
	}
}
