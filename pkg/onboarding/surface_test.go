package onboarding

import (
	"bytes"
	"strings"
	"testing"
)

func emptyEnv(string) string { return "" }

func envFunc(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestPickSurface_NonTTY_ReturnsSilent(t *testing.T) {
	got, err := PickSurface(strings.NewReader(""), &bytes.Buffer{},
		false /*isTTY*/, false /*hasDisplay*/, emptyEnv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != SurfaceSilent {
		t.Errorf("got %v; want SurfaceSilent", got)
	}
}

func TestPickSurface_TTY_NoDisplay_ReturnsCLI(t *testing.T) {
	got, err := PickSurface(strings.NewReader(""), &bytes.Buffer{},
		true /*isTTY*/, false /*hasDisplay*/, emptyEnv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != SurfaceCLI {
		t.Errorf("got %v; want SurfaceCLI", got)
	}
}

func TestPickSurface_TTY_Display_DefaultBrowser(t *testing.T) {
	// Empty stdin line → user accepts default.
	got, err := PickSurface(strings.NewReader("\n"), &bytes.Buffer{},
		true, true, emptyEnv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != SurfaceBrowser {
		t.Errorf("got %v; want SurfaceBrowser", got)
	}
}

func TestPickSurface_TTY_Display_ExplicitCLI(t *testing.T) {
	got, err := PickSurface(strings.NewReader("c\n"), &bytes.Buffer{},
		true, true, emptyEnv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != SurfaceCLI {
		t.Errorf("got %v; want SurfaceCLI", got)
	}
}

func TestPickSurface_TTY_Display_ExplicitBrowser(t *testing.T) {
	got, err := PickSurface(strings.NewReader("b\n"), &bytes.Buffer{},
		true, true, emptyEnv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != SurfaceBrowser {
		t.Errorf("got %v; want SurfaceBrowser", got)
	}
}

func TestPickSurface_EnvOverride_Silent(t *testing.T) {
	// Even with TTY+display, env forces Silent.
	got, err := PickSurface(strings.NewReader("b\n"), &bytes.Buffer{},
		true, true, envFunc(map[string]string{"DICODE_ONBOARDING": "silent"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != SurfaceSilent {
		t.Errorf("got %v; want SurfaceSilent (env override)", got)
	}
}

func TestPickSurface_EnvOverride_CLI(t *testing.T) {
	got, err := PickSurface(strings.NewReader(""), &bytes.Buffer{},
		false /*isTTY*/, false, envFunc(map[string]string{"DICODE_ONBOARDING": "cli"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != SurfaceCLI {
		t.Errorf("got %v; want SurfaceCLI (env override)", got)
	}
}

func TestPickSurface_EnvOverride_Browser(t *testing.T) {
	got, err := PickSurface(strings.NewReader(""), &bytes.Buffer{},
		false, false, envFunc(map[string]string{"DICODE_ONBOARDING": "browser"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != SurfaceBrowser {
		t.Errorf("got %v; want SurfaceBrowser (env override)", got)
	}
}

func TestPickSurface_PromptIsWrittenToOut(t *testing.T) {
	var out bytes.Buffer
	_, err := PickSurface(strings.NewReader("\n"), &out, true, true, emptyEnv)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := strings.ToLower(out.String())
	// Accept either bracketed-letter form ([b]rowser / [c]li) or full words.
	hasBrowser := strings.Contains(got, "[b]") || strings.Contains(got, "browser")
	hasCLI := strings.Contains(got, "[c]") || strings.Contains(got, "cli")
	if !hasBrowser || !hasCLI {
		t.Errorf("prompt missing browser/cli reference: %q", got)
	}
}
