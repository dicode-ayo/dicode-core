package taskset

import (
	"context"
	"testing"
)

func TestSetDevMode_LocalPath_StillWorks(t *testing.T) {
	src := newTestSource(t, "ns", "/tmp/fixture-taskset.yaml")
	ctx := context.Background()

	if err := src.SetDevMode(ctx, true, DevModeOpts{LocalPath: "/tmp/fixture-taskset.yaml"}); err != nil {
		t.Fatalf("enable dev-mode with localPath: %v", err)
	}
	if !src.DevMode() {
		t.Fatal("DevMode() = false after enable, want true")
	}
	if got := src.DevRootPath(); got != "/tmp/fixture-taskset.yaml" {
		t.Errorf("DevRootPath = %q, want /tmp/fixture-taskset.yaml", got)
	}

	if err := src.SetDevMode(ctx, false, DevModeOpts{}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if src.DevMode() {
		t.Fatal("DevMode() = true after disable, want false")
	}
}

func TestSetDevMode_RejectsBothLocalPathAndBranch(t *testing.T) {
	src := newTestSource(t, "ns", "/tmp/fixture-taskset.yaml")
	err := src.SetDevMode(context.Background(), true, DevModeOpts{
		LocalPath: "/tmp/foo", Branch: "fix/x",
	})
	if err == nil {
		t.Fatal("expected error for both LocalPath and Branch set")
	}
}
