package onboarding

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// renderedConfig captures just the pieces of the generated YAML we assert on.
type renderedConfig struct {
	Sources []struct {
		Type      string `yaml:"type"`
		Name      string `yaml:"name"`
		URL       string `yaml:"url"`
		Branch    string `yaml:"branch"`
		EntryPath string `yaml:"entry_path"`
		Path      string `yaml:"path"`
	} `yaml:"sources"`
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
	for _, s := range rc.Sources {
		if s.Type == "git" {
			gitCount++
		}
	}
	if gitCount != 3 {
		t.Errorf("git sources = %d; want 3", gitCount)
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
	for _, s := range rc.Sources {
		if s.Type == "local" {
			localCount++
			if s.Path != "/home/user/dicode-tasks" {
				t.Errorf("local path = %q; want /home/user/dicode-tasks", s.Path)
			}
		}
	}
	if localCount != 1 {
		t.Errorf("local sources = %d; want 1", localCount)
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
	for _, s := range rc.Sources {
		if s.Type == "git" {
			gitNames = append(gitNames, s.Name)
		}
	}
	if len(gitNames) != 1 || gitNames[0] != "buildin" {
		t.Errorf("git source names = %v; want [buildin]", gitNames)
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

	for _, s := range rc.Sources {
		if s.Type == "local" {
			t.Errorf("unexpected local source %+v when LocalTasksDir is empty", s)
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

	var found bool
	for _, s := range rc.Sources {
		if s.Type != "git" {
			continue
		}
		found = true
		var preset TaskSetPreset
		for _, p := range TaskSetPresets {
			if p.Name == "buildin" {
				preset = p
				break
			}
		}
		if s.Name != preset.Name {
			t.Errorf("name = %q; want %q", s.Name, preset.Name)
		}
		if s.URL != preset.URL {
			t.Errorf("url = %q; want %q", s.URL, preset.URL)
		}
		if s.Branch != preset.Branch {
			t.Errorf("branch = %q; want %q", s.Branch, preset.Branch)
		}
		if s.EntryPath != preset.EntryPath {
			t.Errorf("entry_path = %q; want %q", s.EntryPath, preset.EntryPath)
		}
	}
	if !found {
		t.Error("no git source found in output")
	}
}
