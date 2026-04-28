package onboarding

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/config"
)

// TestRenderConfig_LoadsCleanly writes the first-run output through the
// real config.Load path and asserts defaults land where the docs claim.
// Guards against drift between the onboarding template and the Config
// struct — e.g., keys for fields that have since been removed (the old
// direct-AI block: model, api_key_env, base_url) would silently survive
// an unmarshal but then mislead the operator.
func TestRenderConfig_LoadsCleanly(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")

	r := defaultResult(dir, 0)
	yaml := RenderConfig(r)
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("generated config failed to load: %v", err)
	}

	// ai.task should default to buildin/dicodai — config applyDefaults
	// fills it in when the template omits the block.
	if cfg.AI.Task != "buildin/dicodai" {
		t.Errorf("AI.Task = %q, want %q (default should land)", cfg.AI.Task, "buildin/dicodai")
	}

	if len(cfg.Sources) == 0 {
		t.Error("sources should not be empty")
	}
	if cfg.Database.Type != "sqlite" {
		t.Errorf("Database.Type = %q, want sqlite", cfg.Database.Type)
	}

	// Guard: the old direct-AI keys must NOT appear in the template.
	for _, stale := range []string{"api_key_env:", "base_url:", "model:"} {
		if strings.Contains(yaml, stale) {
			t.Errorf("template still contains stale direct-AI key %q; remove it", stale)
		}
	}
}

// TestDefaultResult_HonorsDataDirEnv guards the Docker image's data
// volume contract: `ENV DICODE_DATA_DIR=/data` only persists state if
// silent onboarding writes that path into the generated dicode.yaml.
// Without this, SQLite + sources would land in the container's writable
// layer and `docker rm` would wipe everything (PR #227 bug).
func TestDefaultResult_HonorsDataDirEnv(t *testing.T) {
	t.Run("env set overrides home default", func(t *testing.T) {
		t.Setenv("DICODE_DATA_DIR", "/data")
		r := defaultResult("/home/nonroot", 0)
		if r.DataDir != "/data" {
			t.Errorf("DataDir = %q, want %q (env should win)", r.DataDir, "/data")
		}
	})

	t.Run("env unset falls back to home", func(t *testing.T) {
		if err := os.Unsetenv("DICODE_DATA_DIR"); err != nil {
			t.Fatalf("unsetenv: %v", err)
		}
		r := defaultResult("/home/nonroot", 0)
		want := "/home/nonroot/.dicode"
		if r.DataDir != want {
			t.Errorf("DataDir = %q, want %q (home fallback)", r.DataDir, want)
		}
	})
}

// TestWriteConfig_FileAndParentArePrivate guards the dashboard
// passphrase (embedded as server.secret) from other users on shared
// hosts: the config file must be 0600 and its parent dir must be 0700.
func TestWriteConfig_FileAndParentArePrivate(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "nested", "dir")
	cfg := filepath.Join(parent, "dicode.yaml")

	if err := WriteConfig(cfg, "dummy: true\n"); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	fi, err := os.Stat(cfg)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Errorf("config file perm = %o; want 0600", mode)
	}

	di, err := os.Stat(parent)
	if err != nil {
		t.Fatalf("stat parent: %v", err)
	}
	if mode := di.Mode().Perm(); mode != 0o700 {
		t.Errorf("parent dir perm = %o; want 0700", mode)
	}
}
