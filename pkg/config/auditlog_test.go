package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadConfigString(t *testing.T, content string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "dicode.yaml")
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return Load(cfgPath)
}

func TestAuditLogConfig_DefaultRetention(t *testing.T) {
	cfg, err := loadConfigString(t, "spec:\n  entries: {}\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.AuditLog.RetentionDays != nil {
		t.Errorf("unset retention_days should stay nil, got %d", *cfg.AuditLog.RetentionDays)
	}
	if got := cfg.AuditLog.EffectiveRetentionDays(); got != 30 {
		t.Errorf("EffectiveRetentionDays: got %d, want 30 (default)", got)
	}
}

func TestAuditLogConfig_ExplicitValues(t *testing.T) {
	cfg, err := loadConfigString(t, "spec:\n  entries: {}\naudit_log:\n  retention_days: 7\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.AuditLog.EffectiveRetentionDays(); got != 7 {
		t.Errorf("EffectiveRetentionDays: got %d, want 7", got)
	}

	// Explicit 0 disables pruning — must NOT fall back to the 30-day default.
	cfg, err = loadConfigString(t, "spec:\n  entries: {}\naudit_log:\n  retention_days: 0\n")
	if err != nil {
		t.Fatalf("Load with 0: %v", err)
	}
	if got := cfg.AuditLog.EffectiveRetentionDays(); got != 0 {
		t.Errorf("explicit 0: EffectiveRetentionDays got %d, want 0", got)
	}
}

func TestAuditLogConfig_NegativeRejected(t *testing.T) {
	_, err := loadConfigString(t, "spec:\n  entries: {}\naudit_log:\n  retention_days: -1\n")
	if err == nil {
		t.Fatal("expected error for negative retention_days")
	}
	if !strings.Contains(err.Error(), "audit_log.retention_days") {
		t.Errorf("unexpected error: %v", err)
	}
}
