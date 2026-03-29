package taskset

import (
	"os"
	"path/filepath"
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

func TestLoadTaskSet_EntryRefMissingPath(t *testing.T) {
	dir := t.TempDir()
	content := `
kind: TaskSet
apiVersion: dicode/v1
metadata:
  name: x
spec:
  entries:
    bad:
      ref:
        url: https://github.com/org/repo
`
	p := writeFile(t, dir, "ts.yaml", content)
	_, err := LoadTaskSet(p)
	if err == nil {
		t.Fatal("expected error for ref missing path")
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
