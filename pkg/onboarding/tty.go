package onboarding

import (
	"fmt"
	"io"
	"os"
)

// printPINSecret writes the setup PIN and wizard URL to the user's
// controlling terminal via /dev/tty when available, and falls back to
// fallbackOut (normally os.Stdout) otherwise. This prevents stdout
// redirection — e.g. `dicode daemon >/var/log/dicode.log 2>&1` under
// systemd — from depositing the PIN into a file whose permissions are
// outside this package's control.
//
// Best-effort: if neither /dev/tty nor fallbackOut is writable the PIN
// is lost but the daemon will still time out on resCh and exit cleanly.
func printPINSecret(fallbackOut io.Writer, pin, url string) error {
	msg := fmt.Sprintf(
		"\n  dicode setup PIN: %s\n  Open your browser to %s and enter the PIN above.\n\n",
		pin, url,
	)
	// Try /dev/tty first — bypasses any stdout redirection.
	if tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0); err == nil {
		defer tty.Close()
		_, werr := tty.Write([]byte(msg))
		if werr == nil {
			return nil
		}
	}
	// Fall back to the supplied writer (typically stdout).
	_, err := fallbackOut.Write([]byte(msg))
	return err
}
