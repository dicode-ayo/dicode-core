package onboarding

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// Surface is one of the three modes the onboarding wizard can run in.
type Surface int

const (
	// SurfaceSilent writes a default config without any user interaction.
	// Selected for non-TTY runs (systemd, Docker, CI) so the daemon keeps
	// starting unattended.
	SurfaceSilent Surface = iota

	// SurfaceCLI runs the interactive bufio-based wizard on the current
	// stdin/stdout. Selected when a TTY is present but no display.
	SurfaceCLI

	// SurfaceBrowser runs an ephemeral HTTP wizard and opens the user's
	// default browser to it. Selected when both a TTY and a display are
	// available, or when the user picks it at the initial prompt.
	SurfaceBrowser
)

// PickSurface resolves which wizard surface to run.
//
// Resolution order:
//  1. DICODE_ONBOARDING env var, if set to "silent"/"cli"/"browser", wins.
//  2. Non-TTY stdin → SurfaceSilent.
//  3. TTY without a display → SurfaceCLI.
//  4. TTY with a display → prompt "[b]rowser or [c]li?" (default browser).
func PickSurface(in io.Reader, out io.Writer, isTTY, hasDisplay bool, env func(string) string) (Surface, error) {
	switch strings.ToLower(strings.TrimSpace(env("DICODE_ONBOARDING"))) {
	case "silent":
		return SurfaceSilent, nil
	case "cli":
		return SurfaceCLI, nil
	case "browser":
		return SurfaceBrowser, nil
	}

	if !isTTY {
		return SurfaceSilent, nil
	}
	if !hasDisplay {
		return SurfaceCLI, nil
	}

	fmt.Fprint(out, "Welcome to dicode. Set up in [b]rowser or [c]li? [b] ")
	line, _ := bufio.NewReader(in).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "c", "cli":
		return SurfaceCLI, nil
	default:
		return SurfaceBrowser, nil
	}
}
