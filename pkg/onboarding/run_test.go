package onboarding

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRun_Silent_WritesParseableYAML(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "dicode.yaml")

	opts := RunOptions{
		IsTTY:      false,
		HasDisplay: false,
		In:         strings.NewReader(""),
		Out:        &bytes.Buffer{},
		Home:       dir, // use tempdir as fake home
		Env:        emptyEnv,
	}
	if err := Run(context.Background(), cfg, opts); err != nil {
		t.Fatalf("Run: %v", err)
	}

	b, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	var parsed struct {
		Sources []struct {
			Type string `yaml:"type"`
			Name string `yaml:"name"`
		} `yaml:"sources"`
		Server struct {
			Auth   bool   `yaml:"auth"`
			Secret string `yaml:"secret"`
		} `yaml:"server"`
	}
	if err := yaml.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("YAML parse: %v\n---\n%s", err, b)
	}

	// Silent default enables all three presets.
	gitNames := map[string]bool{}
	for _, s := range parsed.Sources {
		if s.Type == "git" {
			gitNames[s.Name] = true
		}
	}
	for _, p := range TaskSetPresets {
		if !gitNames[p.Name] {
			t.Errorf("silent default missing preset %q", p.Name)
		}
	}
	if !parsed.Server.Auth {
		t.Error("silent default did not set server.auth")
	}
	if len(parsed.Server.Secret) != 24 {
		t.Errorf("silent default passphrase len = %d; want 24", len(parsed.Server.Secret))
	}
}

func TestRun_Silent_PrintsSuccessBanner(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "dicode.yaml")

	var out bytes.Buffer
	opts := RunOptions{
		IsTTY: false, HasDisplay: false,
		In:   strings.NewReader(""),
		Out:  &out,
		Home: dir,
		Env:  emptyEnv,
	}
	if err := Run(context.Background(), cfg, opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "setup complete") {
		t.Errorf("expected success banner, got: %q", s)
	}
	if !strings.Contains(s, "passphrase") {
		t.Errorf("expected 'passphrase' in output, got: %q", s)
	}
}

func TestRun_CLI_DrivesWizard(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "dicode.yaml")

	in := scriptedStdin(
		"y", "y", "y", // all presets
		"", // default local dir
		"", // skip advanced
	)
	opts := RunOptions{
		IsTTY: true, HasDisplay: false, // TTY + no display → CLI
		In:   in,
		Out:  &bytes.Buffer{},
		Home: dir,
		Env:  emptyEnv,
	}
	if err := Run(context.Background(), cfg, opts); err != nil {
		t.Fatalf("Run: %v", err)
	}

	b, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(b), "server:") {
		t.Errorf("config missing server block: %s", b)
	}
}

func TestRun_EnvOverride_Silent_SkipsPrompts(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "dicode.yaml")

	opts := RunOptions{
		IsTTY:      true, // would normally prompt…
		HasDisplay: true,
		In:         strings.NewReader(""), // …but there's no scripted input
		Out:        &bytes.Buffer{},
		Home:       dir,
		Env:        envFunc(map[string]string{"DICODE_ONBOARDING": "silent"}),
	}
	if err := Run(context.Background(), cfg, opts); err != nil {
		t.Fatalf("Run (env override): %v", err)
	}
	if _, err := os.Stat(cfg); err != nil {
		t.Errorf("config not written: %v", err)
	}
}
