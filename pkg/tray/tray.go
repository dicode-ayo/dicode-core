// Package tray manages the system-tray icon for dicode.
// On Linux it uses the StatusNotifierItem DBus protocol (no GTK needed).
// On macOS it uses the native NSStatusItem.
// On Windows it uses the notification area.
//
// Call Run from a goroutine; it blocks until Quit() is called or the context
// is cancelled.
package tray

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"

	"fyne.io/systray"
	"go.uber.org/zap"
)

// Run starts the system-tray icon. It blocks until ctx is cancelled.
// port is the HTTP port dicode is listening on (used to build the dashboard URL).
func Run(ctx context.Context, port int, log *zap.Logger) {
	url := fmt.Sprintf("http://localhost:%d", port)

	onReady := func() {
		systray.SetIcon(iconPNG)
		systray.SetTitle("dicode")
		systray.SetTooltip("dicode — automation server")

		mOpen := systray.AddMenuItem("Open Dashboard", url)
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Quit dicode", "Stop the dicode server")

		// Watch for ctx cancellation so we quit the tray when the server stops.
		go func() {
			<-ctx.Done()
			systray.Quit()
		}()

		// Handle menu clicks.
		go func() {
			for {
				select {
				case <-mOpen.ClickedCh:
					openBrowser(url, log)
				case <-mQuit.ClickedCh:
					systray.Quit()
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	onExit := func() {}

	systray.Run(onReady, onExit)
}

func openBrowser(url string, log *zap.Logger) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		log.Warn("openBrowser: unsupported OS", zap.String("os", runtime.GOOS))
		return
	}
	if err := cmd.Start(); err != nil {
		log.Warn("openBrowser: failed to open", zap.String("url", url), zap.Error(err))
	}
}
