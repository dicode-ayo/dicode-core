package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadConfigString(t *testing.T, content string) (*Config, error) {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "dicode.yaml")
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(cfgPath)
}

func TestApprovalDefaultsEnabled(t *testing.T) {
	cfg, err := loadConfigString(t, "spec:\n  entries: {}\n")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Approval.IsEnabled() {
		t.Fatal("approval gate must default to enabled")
	}
}

func TestApprovalPolicyParsing(t *testing.T) {
	cfg, err := loadConfigString(t, `
spec:
  entries: {}
approval:
  enabled: false
  notify_task: buildin/notify
  sources:
    my-repo: { trust: always }
  tasks:
    other/task: { trust: always }
`)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Approval.IsEnabled() {
		t.Fatal("enabled: false not honoured")
	}
	if cfg.Approval.NotifyTask != "buildin/notify" {
		t.Fatalf("notify_task = %q", cfg.Approval.NotifyTask)
	}
	if cfg.Approval.Sources["my-repo"].Trust != "always" {
		t.Fatalf("sources trust = %+v", cfg.Approval.Sources)
	}
	if cfg.Approval.Tasks["other/task"].Trust != "always" {
		t.Fatalf("tasks trust = %+v", cfg.Approval.Tasks)
	}
}

func TestApprovalInvalidTrustRejected(t *testing.T) {
	for _, block := range []string{
		"approval:\n  sources:\n    repo: { trust: sometimes }\n",
		"approval:\n  tasks:\n    repo/task: { trust: never }\n",
	} {
		_, err := loadConfigString(t, "spec:\n  entries: {}\n"+block)
		if err == nil || !strings.Contains(err.Error(), "trust") {
			t.Fatalf("invalid trust value not rejected: %v", err)
		}
	}
}
