// Package tui provides an interactive terminal dashboard for dicode.
// It connects to a running dicode server over HTTP and provides
// task monitoring, run history, and log inspection via keyboard navigation.
//
// Launch with: dicode tui [--host localhost] [--port 8080]
package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
)

// Run starts the TUI and blocks until the user quits.
// It connects to the dicode server at host:port.
func Run(host string, port int) error {
	m := newModel(host, port)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}
