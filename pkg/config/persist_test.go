package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const baseYAML = `apiVersion: dicode/v1
spec:
  entries:
    buildin:
      ref:
        path: /tmp/tasks/buildin/taskset.yaml
    examples:
      ref:
        url: https://example.com/repo
        path: tasks/examples/taskset.yaml
log_level: info
`

func writeTempYAML(t *testing.T, content string) (string, time.Time) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "dicode.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	return p, fi.ModTime()
}

func TestMergeTaskOverride_CreatesEnabledFalse(t *testing.T) {
	p, mt := writeTempYAML(t, baseYAML)
	if err := MergeTaskOverride(p, "buildin/relay-client", []byte(`{"enabled": false}`), mt); err != nil {
		t.Fatalf("merge: %v", err)
	}
	got, _ := os.ReadFile(p)
	want := []string{
		"buildin:",
		"overrides:",
		"entries:",
		"relay-client:",
		"enabled: false",
	}
	for _, w := range want {
		if !strings.Contains(string(got), w) {
			t.Errorf("output missing %q\n--- got ---\n%s", w, got)
		}
	}
}

func TestMergeTaskOverride_OverwritesExisting(t *testing.T) {
	yaml := strings.Replace(baseYAML, "    buildin:\n      ref:\n        path: /tmp/tasks/buildin/taskset.yaml",
		`    buildin:
      ref:
        path: /tmp/tasks/buildin/taskset.yaml
      overrides:
        entries:
          relay-client:
            enabled: false`, 1)
	p, mt := writeTempYAML(t, yaml)
	if err := MergeTaskOverride(p, "buildin/relay-client", []byte(`{"enabled": true}`), mt); err != nil {
		t.Fatalf("merge: %v", err)
	}
	got, _ := os.ReadFile(p)
	if !strings.Contains(string(got), "enabled: true") {
		t.Errorf("expected enabled: true in output\n%s", got)
	}
	if strings.Contains(string(got), "enabled: false") {
		t.Errorf("old enabled: false should be gone\n%s", got)
	}
}

func TestMergeTaskOverride_NullDeletesField(t *testing.T) {
	yaml := strings.Replace(baseYAML, "    buildin:\n      ref:\n        path: /tmp/tasks/buildin/taskset.yaml",
		`    buildin:
      ref:
        path: /tmp/tasks/buildin/taskset.yaml
      overrides:
        entries:
          x:
            enabled: false
            timeout: 5m`, 1)
	p, mt := writeTempYAML(t, yaml)
	if err := MergeTaskOverride(p, "buildin/x", []byte(`{"enabled": null}`), mt); err != nil {
		t.Fatalf("merge: %v", err)
	}
	got, _ := os.ReadFile(p)
	if strings.Contains(string(got), "enabled:") {
		t.Errorf("expected 'enabled' to be deleted by null patch; got:\n%s", got)
	}
	if !strings.Contains(string(got), "timeout: 5m") {
		t.Errorf("expected 'timeout: 5m' to be preserved; got:\n%s", got)
	}
}

func TestMergeTaskOverride_PrunesEmptyOverrides(t *testing.T) {
	yaml := strings.Replace(baseYAML, "    buildin:\n      ref:\n        path: /tmp/tasks/buildin/taskset.yaml",
		`    buildin:
      ref:
        path: /tmp/tasks/buildin/taskset.yaml
      overrides:
        entries:
          x:
            enabled: false`, 1)
	p, mt := writeTempYAML(t, yaml)
	if err := MergeTaskOverride(p, "buildin/x", []byte(`{"enabled": null}`), mt); err != nil {
		t.Fatalf("merge: %v", err)
	}
	got, _ := os.ReadFile(p)
	if strings.Contains(string(got), "overrides:") {
		t.Errorf("empty overrides block should be pruned; got:\n%s", got)
	}
}

func TestMergeTaskOverride_GenericFields(t *testing.T) {
	p, mt := writeTempYAML(t, baseYAML)
	patch := []byte(`{"params": {"model": "gpt-4o"}, "timeout": "5m"}`)
	if err := MergeTaskOverride(p, "buildin/dicodai", patch, mt); err != nil {
		t.Fatalf("merge: %v", err)
	}
	got, _ := os.ReadFile(p)
	for _, w := range []string{"params:", "model: gpt-4o", "timeout: 5m"} {
		if !strings.Contains(string(got), w) {
			t.Errorf("missing %q\n%s", w, got)
		}
	}
}

func TestMergeTaskOverride_MtimeMismatchRejects(t *testing.T) {
	p, _ := writeTempYAML(t, baseYAML)
	staleMtime := time.Now().Add(-time.Hour)
	err := MergeTaskOverride(p, "buildin/x", []byte(`{"enabled": false}`), staleMtime)
	if !errors.Is(err, ErrConcurrentModification) {
		t.Fatalf("err = %v, want ErrConcurrentModification", err)
	}
}

func TestMergeTaskOverride_UnknownSourceErrors(t *testing.T) {
	p, mt := writeTempYAML(t, baseYAML)
	err := MergeTaskOverride(p, "nonexistent/x", []byte(`{"enabled": false}`), mt)
	if err == nil {
		t.Fatal("want error for unknown source key")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error should mention source name; got: %v", err)
	}
}

func TestMergeTaskOverride_AtomicWrite(t *testing.T) {
	p, mt := writeTempYAML(t, baseYAML)
	if err := MergeTaskOverride(p, "buildin/x", []byte(`{"enabled": false}`), mt); err != nil {
		t.Fatalf("merge: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(p))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".dicode.yaml.tmp") || strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

// TestMergeTaskOverride_PreservesPinnedTag guards the pin against the
// whole-document rewrite MergeTaskOverride performs: a source pinned to a tag
// must come back out of that round-trip still pinned, and still without the
// branch the loader defaults onto unpinned git refs.
func TestMergeTaskOverride_PreservesPinnedTag(t *testing.T) {
	pinned := strings.Replace(baseYAML,
		"        url: https://example.com/repo\n",
		"        url: https://example.com/repo\n        tag: v1.2.3\n", 1)
	p, mt := writeTempYAML(t, pinned)

	if err := MergeTaskOverride(p, "examples/hello", []byte(`{"enabled": false}`), mt); err != nil {
		t.Fatalf("merge: %v", err)
	}

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	ref := cfg.Spec.Entries["examples"].Ref
	if ref.Tag != "v1.2.3" {
		t.Errorf("ref.tag = %q after round-trip, want %q", ref.Tag, "v1.2.3")
	}
	if ref.Branch != "" {
		t.Errorf("ref.branch = %q on a pinned ref, want it left unset", ref.Branch)
	}
}
