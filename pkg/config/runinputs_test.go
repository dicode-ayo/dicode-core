package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestRunInputsConfig_Defaults(t *testing.T) {
	cfg := &Config{}
	applyDefaults(cfg, "/tmp/cfg")
	if cfg.Defaults.RunInputs.Retention != 30*24*time.Hour {
		t.Errorf("retention default wrong: %v", cfg.Defaults.RunInputs.Retention)
	}
	if cfg.Defaults.RunInputs.StorageTask != "buildin/local-storage" {
		t.Errorf("storage_task default wrong: %v", cfg.Defaults.RunInputs.StorageTask)
	}
	if !cfg.Defaults.RunInputs.IsEnabled() {
		t.Error("default should be enabled")
	}
}

func TestRunInputsConfig_ParsesYAML(t *testing.T) {
	yamlSrc := []byte(`
defaults:
  run_inputs:
    enabled: false
    retention: 24h
    storage_task: my-s3
    body_full_textual: true
`)
	var cfg Config
	if err := yaml.Unmarshal(yamlSrc, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.RunInputs.IsEnabled() {
		t.Error("enabled=false not parsed")
	}
	if cfg.Defaults.RunInputs.Retention != 24*time.Hour {
		t.Errorf("retention = %v", cfg.Defaults.RunInputs.Retention)
	}
	if cfg.Defaults.RunInputs.StorageTask != "my-s3" {
		t.Errorf("storage_task = %v", cfg.Defaults.RunInputs.StorageTask)
	}
	if !cfg.Defaults.RunInputs.BodyFullTextual {
		t.Error("body_full_textual not parsed")
	}
}

func TestRunInputsConfig_RetentionDoesNotOverrideExplicit(t *testing.T) {
	cfg := &Config{}
	cfg.Defaults.RunInputs.Retention = 7 * 24 * time.Hour
	applyDefaults(cfg, "/tmp/cfg")
	if cfg.Defaults.RunInputs.Retention != 7*24*time.Hour {
		t.Errorf("explicit retention overridden: %v", cfg.Defaults.RunInputs.Retention)
	}
}

func TestRunInputsConfig_StorageTaskDoesNotOverrideExplicit(t *testing.T) {
	cfg := &Config{}
	cfg.Defaults.RunInputs.StorageTask = "my-task"
	applyDefaults(cfg, "/tmp/cfg")
	if cfg.Defaults.RunInputs.StorageTask != "my-task" {
		t.Errorf("explicit storage_task overridden: %v", cfg.Defaults.RunInputs.StorageTask)
	}
}
