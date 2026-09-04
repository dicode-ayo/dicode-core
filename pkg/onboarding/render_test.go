package onboarding

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// renderedEntry captures one spec.entries entry from the generated YAML.
type renderedEntry struct {
	Ref struct {
		URL    string `yaml:"url"`
		Branch string `yaml:"branch"`
		Tag    string `yaml:"tag"`
		Path   string `yaml:"path"`
	} `yaml:"ref"`
}

// renderedConfig captures just the pieces of the generated YAML we assert on.
// The new shape uses spec.entries (a map keyed by source name) instead of the
// old sources[] array.
type renderedConfig struct {
	Spec struct {
		Entries map[string]renderedEntry `yaml:"entries"`
	} `yaml:"spec"`
	Server struct {
		Auth   bool   `yaml:"auth"`
		Secret string `yaml:"secret"`
		Port   int    `yaml:"port"`
	} `yaml:"server"`
}

func parseRendered(t *testing.T, out string) renderedConfig {
	t.Helper()
	var rc renderedConfig
	if err := yaml.Unmarshal([]byte(out), &rc); err != nil {
		t.Fatalf("generated YAML does not parse: %v\n---\n%s", err, out)
	}
	return rc
}

func allPresetsEnabled() map[string]bool {
	m := make(map[string]bool, len(TaskSetPresets))
	for _, p := range TaskSetPresets {
		m[p.Name] = true
	}
	return m
}

func TestRenderConfig_AllTasksetsEnabled_ThreeGitSources(t *testing.T) {
	r := Result{
		TaskSetsEnabled: allPresetsEnabled(),
		LocalTasksDir:   "/home/user/dicode-tasks",
		DataDir:         "/home/user/.dicode",
		Port:            8080,
		Passphrase:      "pW9mX3kL2nQ7vR4tY8uI3oP6",
	}
	rc := parseRendered(t, RenderConfig(r))

	gitCount := 0
	for _, e := range rc.Spec.Entries {
		if e.Ref.URL != "" {
			gitCount++
		}
	}
	if gitCount != 3 {
		t.Errorf("git entries = %d; want 3", gitCount)
	}
}

func TestRenderConfig_AllTasksetsEnabled_IncludesLocalSource(t *testing.T) {
	r := Result{
		TaskSetsEnabled: allPresetsEnabled(),
		LocalTasksDir:   "/home/user/dicode-tasks",
		DataDir:         "/home/user/.dicode",
		Port:            8080,
		Passphrase:      "p",
	}
	rc := parseRendered(t, RenderConfig(r))

	localCount := 0
	for _, e := range rc.Spec.Entries {
		if e.Ref.URL == "" && e.Ref.Path != "" {
			localCount++
			if e.Ref.Path != "/home/user/dicode-tasks" {
				t.Errorf("local path = %q; want /home/user/dicode-tasks", e.Ref.Path)
			}
		}
	}
	if localCount != 1 {
		t.Errorf("local entries = %d; want 1", localCount)
	}
}

func TestRenderConfig_ServerAuthAndSecret(t *testing.T) {
	r := Result{
		TaskSetsEnabled: allPresetsEnabled(),
		DataDir:         "/tmp/d",
		Port:            8080,
		Passphrase:      "secret-passphrase-test-1",
	}
	rc := parseRendered(t, RenderConfig(r))

	if !rc.Server.Auth {
		t.Error("server.auth = false; want true")
	}
	if rc.Server.Secret != "secret-passphrase-test-1" {
		t.Errorf("server.secret = %q; want %q", rc.Server.Secret, r.Passphrase)
	}
	if rc.Server.Port != 8080 {
		t.Errorf("server.port = %d; want 8080", rc.Server.Port)
	}
}

func TestRenderConfig_PartialSelection_DropsUnselected(t *testing.T) {
	r := Result{
		TaskSetsEnabled: map[string]bool{"buildin": true, "examples": false, "auth": false},
		LocalTasksDir:   "/tmp/t",
		DataDir:         "/tmp/d",
		Port:            8080,
		Passphrase:      "p",
	}
	rc := parseRendered(t, RenderConfig(r))

	var gitNames []string
	for name, e := range rc.Spec.Entries {
		if e.Ref.URL != "" {
			gitNames = append(gitNames, name)
		}
	}
	if len(gitNames) != 1 || gitNames[0] != "buildin" {
		t.Errorf("git entry names = %v; want [buildin]", gitNames)
	}
}

func TestRenderConfig_EmptyLocalTasksDir_OmitsLocalSource(t *testing.T) {
	r := Result{
		TaskSetsEnabled: map[string]bool{"buildin": true},
		LocalTasksDir:   "",
		DataDir:         "/tmp/d",
		Port:            8080,
		Passphrase:      "p",
	}
	rc := parseRendered(t, RenderConfig(r))

	for name, e := range rc.Spec.Entries {
		if e.Ref.URL == "" && e.Ref.Path != "" {
			t.Errorf("unexpected local entry %q when LocalTasksDir is empty", name)
		}
	}
}

func TestRenderConfig_GitSourceFieldsMatchPreset(t *testing.T) {
	r := Result{
		TaskSetsEnabled: map[string]bool{"buildin": true},
		DataDir:         "/tmp/d",
		Port:            8080,
		Passphrase:      "p",
	}
	rc := parseRendered(t, RenderConfig(r))

	e, found := rc.Spec.Entries["buildin"]
	if !found {
		t.Fatal("buildin entry not found in spec.entries")
	}
	var preset TaskSetPreset
	for _, p := range TaskSetPresets {
		if p.Name == "buildin" {
			preset = p
			break
		}
	}
	if e.Ref.URL != preset.URL {
		t.Errorf("url = %q; want %q", e.Ref.URL, preset.URL)
	}
	if e.Ref.Branch != preset.Branch {
		t.Errorf("branch = %q; want %q", e.Ref.Branch, preset.Branch)
	}
	if e.Ref.Path != preset.EntryPath {
		t.Errorf("path = %q; want %q", e.Ref.Path, preset.EntryPath)
	}
}

// TestRenderConfig_YAMLInjectionSafe verifies that newlines in DataDir or
// LocalTasksDir cannot inject extra YAML keys.
func TestRenderConfig_YAMLInjectionSafe(t *testing.T) {
	r := Result{
		TaskSetsEnabled: map[string]bool{},
		LocalTasksDir:   "/safe/path\nmalicious: injected",
		DataDir:         "/data\nevil: true",
		Port:            8080,
		Passphrase:      "p",
	}
	out := RenderConfig(r)
	// The output must parse as valid YAML.
	var doc map[string]interface{}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("RenderConfig produced invalid YAML: %v\n%s", err, out)
	}
	// The injected keys must not appear at the top level.
	if _, ok := doc["malicious"]; ok {
		t.Error("YAML injection via LocalTasksDir succeeded")
	}
	if _, ok := doc["evil"]; ok {
		t.Error("YAML injection via DataDir succeeded")
	}
}

// TestRenderConfig_PinnedPresetRendersTagNotBranch keeps a pinned preset's
// generated entry loadable: rendering both keys would make the wizard's own
// output a config-load error.
func TestRenderConfig_PinnedPresetRendersTagNotBranch(t *testing.T) {
	original := TaskSetPresets
	t.Cleanup(func() { TaskSetPresets = original })
	TaskSetPresets = []TaskSetPreset{{
		Name:      "buildin",
		URL:       "https://example.com/repo",
		Tag:       "v0.1.0",
		EntryPath: "taskset.yaml",
	}}

	rc := parseRendered(t, RenderConfig(Result{
		TaskSetsEnabled: map[string]bool{"buildin": true},
		DataDir:         "/tmp/d",
		Port:            8080,
		Passphrase:      "p",
	}))

	ref := rc.Spec.Entries["buildin"].Ref
	if ref.Tag != "v0.1.0" {
		t.Errorf("rendered tag = %q, want %q", ref.Tag, "v0.1.0")
	}
	if ref.Branch != "" {
		t.Errorf("rendered branch = %q on a pinned preset, want it omitted", ref.Branch)
	}
}
