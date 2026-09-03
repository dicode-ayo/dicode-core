package taskset

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestDetectKind(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		name    string
		content string
		want    Kind
		wantErr bool
	}{
		{
			name:    "TaskSet",
			content: "kind: TaskSet\napiVersion: dicode/v1\n",
			want:    KindTaskSet,
		},
		{
			name:    "Task",
			content: "kind: Task\napiVersion: dicode/v1\n",
			want:    KindTask,
		},
		{
			name:    "Config",
			content: "kind: Config\napiVersion: dicode/v1\n",
			want:    KindConfig,
		},
		{
			name:    "missing kind",
			content: "apiVersion: dicode/v1\n",
			wantErr: true,
		},
		{
			name:    "invalid yaml",
			content: ":\n  bad: [yaml",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := writeFile(t, dir, tc.name+".yaml", tc.content)
			got, err := DetectKind(p)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDetectKind_FileNotFound(t *testing.T) {
	_, err := DetectKind("/does/not/exist.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadTaskSet_Valid(t *testing.T) {
	dir := t.TempDir()
	content := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: test
spec:
  defaults:
    timeout: 60s
    env:
      - LOG=info
  entries:
    deploy:
      ref:
        url: https://github.com/org/tasks
        path: tasks/deploy/task.yaml
      overrides:
        enabled: true
        trigger:
          cron: "0 2 * * *"
    health-check:
      inline:
        name: Health Check
        runtime: deno
        trigger:
          manual: true
`
	p := writeFile(t, dir, "taskset.yaml", content)
	ts, err := LoadTaskSet(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ts.Metadata.Name != "test" {
		t.Errorf("name: got %q, want %q", ts.Metadata.Name, "test")
	}
	if len(ts.Spec.Entries) != 2 {
		t.Errorf("entries: got %d, want 2", len(ts.Spec.Entries))
	}
	if ts.Spec.Defaults == nil {
		t.Error("defaults should not be nil")
	}
	if ts.Spec.Defaults.Timeout.String() != "1m0s" {
		t.Errorf("defaults.timeout: got %v", ts.Spec.Defaults.Timeout)
	}

	deploy := ts.Spec.Entries["deploy"]
	if deploy == nil {
		t.Fatal("deploy entry missing")
	}
	if deploy.Ref == nil {
		t.Fatal("deploy.ref is nil")
	}
	if deploy.Ref.URL != "https://github.com/org/tasks" {
		t.Errorf("deploy.ref.url: got %q", deploy.Ref.URL)
	}
	if deploy.Overrides == nil || deploy.Overrides.Trigger == nil {
		t.Fatal("deploy overrides/trigger missing")
	}
	if deploy.Overrides.Trigger.Cron == nil || *deploy.Overrides.Trigger.Cron != "0 2 * * *" {
		t.Errorf("deploy trigger.cron wrong")
	}

	hc := ts.Spec.Entries["health-check"]
	if hc == nil {
		t.Fatal("health-check entry missing")
	}
	if hc.Inline == nil {
		t.Fatal("health-check.inline is nil")
	}
}

func TestLoadTaskSet_WrongKind(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "ts.yaml", "kind: Config\nspec:\n  entries: {}\n")
	_, err := LoadTaskSet(p)
	if err == nil {
		t.Fatal("expected error for wrong kind")
	}
}

func TestLoadTaskSet_MissingEntries(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "ts.yaml", "kind: TaskSet\napiVersion: dicode/v1\nmetadata:\n  name: x\nspec:\n  defaults:\n    timeout: 10s\n")
	_, err := LoadTaskSet(p)
	if err == nil {
		t.Fatal("expected error for missing entries")
	}
}

// TestLoadTaskSet_RejectsLegacyNotify_AtDefaults ensures a top-level
// `defaults.notify:` block is rejected at load time (#279).
func TestLoadTaskSet_RejectsLegacyNotify_AtDefaults(t *testing.T) {
	dir := t.TempDir()
	content := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: test
spec:
  defaults:
    notify:
      on_success: false
      on_failure: true
  entries:
    sample:
      ref:
        path: ./sample
`
	p := writeFile(t, dir, "ts.yaml", content)
	_, err := LoadTaskSet(p)
	if err == nil {
		t.Fatal("LoadTaskSet accepted legacy defaults.notify; want error")
	}
	if !strings.Contains(err.Error(), "on_failure_chain") {
		t.Errorf("error = %v; want mention of on_failure_chain migration", err)
	}
}

// TestLoadTaskSet_RejectsLegacyNotify_NestedOverride ensures a notify
// block buried under entries.<key>.overrides is also caught (#279).
func TestLoadTaskSet_RejectsLegacyNotify_NestedOverride(t *testing.T) {
	dir := t.TempDir()
	content := `
apiVersion: dicode/v1
kind: TaskSet
metadata:
  name: test
spec:
  entries:
    sample:
      ref:
        path: ./sample
      overrides:
        notify:
          on_failure: true
`
	p := writeFile(t, dir, "ts.yaml", content)
	_, err := LoadTaskSet(p)
	if err == nil {
		t.Fatal("LoadTaskSet accepted nested overrides.notify; want error")
	}
	if !strings.Contains(err.Error(), "on_failure_chain") {
		t.Errorf("error = %v; want mention of on_failure_chain migration", err)
	}
}

func TestLoadTaskSet_EntryMissingRefAndInline(t *testing.T) {
	dir := t.TempDir()
	content := `
kind: TaskSet
apiVersion: dicode/v1
metadata:
  name: x
spec:
  entries:
    bad: {}
`
	p := writeFile(t, dir, "ts.yaml", content)
	_, err := LoadTaskSet(p)
	if err == nil {
		t.Fatal("expected error for entry missing ref and inline")
	}
}

// TestLoadTaskSet_EntryRefMissingPath checks that:
//   - a git ref (URL non-empty) with no path is valid — the source manager
//     treats an empty path as "taskset.yaml at the repo root".
//   - a local ref (URL empty) with no path is still rejected.
func TestLoadTaskSet_EntryRefMissingPath(t *testing.T) {
	dir := t.TempDir()

	// Git ref with empty path — must succeed.
	gitContent := `
kind: TaskSet
apiVersion: dicode/v1
metadata:
  name: x
spec:
  entries:
    root-ref:
      ref:
        url: https://github.com/org/repo
`
	p := writeFile(t, dir, "ts_git.yaml", gitContent)
	if _, err := LoadTaskSet(p); err != nil {
		t.Errorf("git ref with empty path should be valid, got: %v", err)
	}

	// Local ref with empty path — must be rejected.
	localContent := `
kind: TaskSet
apiVersion: dicode/v1
metadata:
  name: x
spec:
  entries:
    bad:
      ref: {}
`
	p2 := writeFile(t, dir, "ts_local.yaml", localContent)
	_, err := LoadTaskSet(p2)
	if err == nil {
		t.Fatal("expected error for local ref missing path")
	}
	if !strings.Contains(err.Error(), "ref.path is required for local refs") {
		t.Errorf("expected 'ref.path is required for local refs' in error, got: %v", err)
	}
}

func TestLoadTaskSet_EntryEnabledShortcut(t *testing.T) {
	dir := t.TempDir()
	content := `
kind: TaskSet
apiVersion: dicode/v1
metadata:
  name: x
spec:
  entries:
    on-task:
      enabled: true
      inline:
        name: a
        runtime: deno
        trigger: { manual: true }
        timeout: 5s
    off-task:
      enabled: false
      inline:
        name: b
        runtime: deno
        trigger: { manual: true }
        timeout: 5s
`
	p := writeFile(t, dir, "ts.yaml", content)
	ts, err := LoadTaskSet(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	on := ts.Spec.Entries["on-task"]
	if on.Enabled != nil {
		t.Errorf("on-task: Enabled should be lifted to Overrides.Enabled, got top-level set")
	}
	if on.Overrides == nil || on.Overrides.Enabled == nil || *on.Overrides.Enabled != true {
		t.Errorf("on-task: expected overrides.enabled=true after lift, got %+v", on.Overrides)
	}
	off := ts.Spec.Entries["off-task"]
	if off.Overrides == nil || off.Overrides.Enabled == nil || *off.Overrides.Enabled != false {
		t.Errorf("off-task: expected overrides.enabled=false after lift, got %+v", off.Overrides)
	}
}

func TestLoadTaskSet_EntryEnabledConflict(t *testing.T) {
	dir := t.TempDir()
	content := `
kind: TaskSet
apiVersion: dicode/v1
metadata:
  name: x
spec:
  entries:
    bad:
      enabled: false
      overrides:
        enabled: true
      inline:
        name: a
        runtime: deno
        trigger: { manual: true }
        timeout: 5s
`
	p := writeFile(t, dir, "ts.yaml", content)
	_, err := LoadTaskSet(p)
	if err == nil {
		t.Fatal("expected conflict error when both top-level enabled and overrides.enabled are set")
	}
	if !strings.Contains(err.Error(), "conflicts with `overrides.enabled`") {
		t.Errorf("expected conflict error, got: %v", err)
	}
}

func TestLoadTaskSet_RefURLSchemes(t *testing.T) {
	dir := t.TempDir()

	mkYAML := func(u string) string {
		return `
kind: TaskSet
apiVersion: dicode/v1
metadata:
  name: x
spec:
  entries:
    entry:
      ref:
        url: ` + u + `
`
	}

	allowed := []string{
		"https://github.com/org/repo",
		"http://internal.corp/tasks",
		"ssh://git@github.com/org/repo.git",
		"git@github.com:org/repo.git",       // SSH shorthand
		"git@gitlab.com:group/subgroup.git", // SSH shorthand with subgroup
	}
	for _, u := range allowed {
		t.Run("allowed:"+u, func(t *testing.T) {
			p := writeFile(t, dir, "ts_allowed.yaml", mkYAML(u))
			if _, err := LoadTaskSet(p); err != nil {
				t.Errorf("expected %q to be allowed, got error: %v", u, err)
			}
		})
	}

	// git:// is rejected (#486): go-git dials it via a native transport with a
	// hardcoded net.Dial and no injectable dialer, so unlike http/https it gets
	// no SSRF host validation at any layer.
	rejected := []string{
		"file:///etc/passwd",
		"file://localhost/tmp/tasks",
		"/tmp/local-path",
		"git://github.com/org/repo.git",
		"git://169.254.169.254/metadata",
	}
	for _, u := range rejected {
		t.Run("rejected:"+u, func(t *testing.T) {
			p := writeFile(t, dir, "ts_rejected.yaml", mkYAML(u))
			_, err := LoadTaskSet(p)
			if err == nil {
				t.Errorf("expected %q to be rejected, got nil error", u)
			}
		})
	}
}

// TestValidateRefURL_GitSchemeRejected pins the exact error message for the
// git:// rejection (#486), so a future refactor that silently drops the
// dedicated git-scheme branch (e.g. folding it back into the generic
// default case) is caught even if the generic case still errors.
func TestValidateRefURL_GitSchemeRejected(t *testing.T) {
	err := ValidateRefURL("test.yaml", "key", "git://github.com/org/repo.git")
	if err == nil {
		t.Fatal("expected git:// scheme to be rejected")
	}
	if !strings.Contains(err.Error(), "no longer accepted") || !strings.Contains(err.Error(), "SSRF") {
		t.Errorf("expected error to explain the SSRF-related rejection, got: %v", err)
	}
}

func TestLoadConfig_Valid(t *testing.T) {
	dir := t.TempDir()
	content := `
apiVersion: dicode/v1
kind: Config
metadata:
  name: my-config
spec:
  runtimes:
    deno:
      version: "2.1.0"
  defaults:
    timeout: 120s
    retry:
      attempts: 3
      backoff: 10s
    env:
      - RUNTIME_ENV=prod
`
	p := writeFile(t, dir, "config.yaml", content)
	cs, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cs == nil {
		t.Fatal("expected non-nil config")
	}
	if cs.Metadata.Name != "my-config" {
		t.Errorf("name: got %q", cs.Metadata.Name)
	}
	if cs.Spec.Defaults == nil {
		t.Fatal("defaults nil")
	}
	if cs.Spec.Defaults.Retry == nil || cs.Spec.Defaults.Retry.Attempts != 3 {
		t.Error("retry.attempts wrong")
	}
	pin, ok := cs.Spec.Runtimes["deno"]
	if !ok || pin.Version != "2.1.0" {
		t.Error("runtime pin wrong")
	}
}

func TestLoadConfig_NotExist(t *testing.T) {
	cs, err := LoadConfig("/does/not/exist/config.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cs != nil {
		t.Error("expected nil for non-existent config")
	}
}

func TestLoadConfig_WrongKind(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "cfg.yaml", "kind: TaskSet\n")
	_, err := LoadConfig(p)
	if err == nil {
		t.Fatal("expected error for wrong kind")
	}
}

// TestValidateRefURL_SCPInvalidHost ensures the SCP fast-path rejects
// syntactically invalid hostnames (e.g. containing path separators or
// shell metacharacters).
func TestValidateRefURL_SCPInvalidHost(t *testing.T) {
	cases := []struct {
		rawURL string
		valid  bool
	}{
		{"git@github.com:org/repo.git", true},
		{"user@host.example.com:path/to/repo", true},
		{"git@github.com:../../etc/passwd", true},     // valid host, path traversal is separate concern
		{"user@git_server.internal:repo", true},       // underscore in hostname (common in corp setups)
		{"user@host_with_underscores:repo.git", true}, // underscores allowed
		{"user@host with spaces:repo", false},         // space in host
		{"user@:repo", false},                         // empty host
	}
	for _, tc := range cases {
		err := ValidateRefURL("test.yaml", "key", tc.rawURL)
		if tc.valid && err != nil {
			t.Errorf("ValidateRefURL(%q) = %v, want nil", tc.rawURL, err)
		}
		if !tc.valid && err == nil {
			t.Errorf("ValidateRefURL(%q) = nil, want error", tc.rawURL)
		}
	}
}

// TestLoadTaskSet_RejectsBranchAndTagTogether keeps a nested taskset's refs
// under the same rule dicode.yaml's are: a ref cannot both track a branch and
// pin a tag, and the error names both fields so the operator knows which to
// drop.
func TestLoadTaskSet_RejectsBranchAndTagTogether(t *testing.T) {
	content := `
kind: TaskSet
apiVersion: dicode/v1
metadata:
  name: x
spec:
  entries:
    pinned:
      ref:
        url: https://github.com/org/repo
        branch: main
        tag: v1.0.0
`
	p := writeFile(t, t.TempDir(), "ts.yaml", content)
	_, err := LoadTaskSet(p)
	if err == nil {
		t.Fatal("LoadTaskSet = nil, want a branch/tag mutual-exclusion error")
	}
	for _, want := range []string{"ref.branch", "ref.tag", "pinned"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestLoadTaskSet_AcceptsTagAlone is the other half: pinning is a legal shape,
// not merely one that fails differently.
func TestLoadTaskSet_AcceptsTagAlone(t *testing.T) {
	content := `
kind: TaskSet
apiVersion: dicode/v1
metadata:
  name: x
spec:
  entries:
    pinned:
      ref:
        url: https://github.com/org/repo
        tag: v1.0.0
`
	p := writeFile(t, t.TempDir(), "ts.yaml", content)
	ts, err := LoadTaskSet(p)
	if err != nil {
		t.Fatalf("LoadTaskSet: %v", err)
	}
	if got := ts.Spec.Entries["pinned"].Ref.Tag; got != "v1.0.0" {
		t.Errorf("ref.tag = %q, want %q", got, "v1.0.0")
	}
}
