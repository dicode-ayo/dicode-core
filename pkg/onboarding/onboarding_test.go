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

	r := defaultResult(dir)
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
