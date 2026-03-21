// Package service manages dicode as an OS-level service or startup item.
//
// Desktop mode  — writes a run-on-login entry (LaunchAgent / XDG autostart / Registry)
// Headless mode — installs a system service (systemd / Windows Service)
//
// Usage:
//
//	dicode service install [--headless]
//	dicode service uninstall
//	dicode service start / stop / restart / status / logs
package service

// Manager installs and controls the dicode OS service.
// The concrete implementation is platform-specific (see service_linux.go,
// service_darwin.go, service_windows.go).
type Manager interface {
	// Install registers dicode as a startup item or system service.
	// headless=true installs a system service; false installs a login item.
	Install(execPath string, headless bool) error

	// Uninstall removes the service/startup entry.
	Uninstall() error

	Start() error
	Stop() error
	Restart() error

	// Status returns a human-readable status string.
	Status() (string, error)

	// Logs streams recent log lines to stdout.
	Logs(lines int) error
}
