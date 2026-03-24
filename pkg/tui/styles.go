package tui

import "github.com/charmbracelet/lipgloss"

var (
	// Colors
	colPurple      = lipgloss.Color("#7c3aed")
	colGreen       = lipgloss.Color("#22c55e")
	colRed         = lipgloss.Color("#ef4444")
	colAmber       = lipgloss.Color("#f59e0b")
	colGray        = lipgloss.Color("#6b7280")
	colDimGray     = lipgloss.Color("#374151")
	colIndigo      = lipgloss.Color("#818cf8")
	colWhite       = lipgloss.Color("#f9fafb")
	colSelectedBg  = lipgloss.Color("#1e1b4b")
	colSelectedFg  = lipgloss.Color("#e0e7ff")
	colLogInfo     = lipgloss.Color("#94a3b8")
	colLogWarn     = lipgloss.Color("#fbbf24")
	colLogError    = lipgloss.Color("#f87171")
	colLogDebug    = lipgloss.Color("#64748b")
	colHeaderBg    = lipgloss.Color("#0f172a")

	// Panel borders
	activeBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colPurple)

	inactiveBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colDimGray)

	// Header bar
	headerStyle = lipgloss.NewStyle().
			Background(colHeaderBg).
			Foreground(colWhite).
			Bold(true).
			Padding(0, 1)

	dimStyle = lipgloss.NewStyle().Foreground(colGray)

	// Task list row
	taskRowStyle = lipgloss.NewStyle().
			PaddingLeft(1)

	taskRowSelectedStyle = lipgloss.NewStyle().
				Background(colSelectedBg).
				Foreground(colSelectedFg).
				PaddingLeft(1).
				Bold(true)

	taskNameStyle      = lipgloss.NewStyle().Foreground(colWhite).Bold(true)
	taskTriggerStyle   = lipgloss.NewStyle().Foreground(colIndigo)

	// Run list row
	runRowStyle = lipgloss.NewStyle().
			PaddingLeft(1)

	runRowSelectedStyle = lipgloss.NewStyle().
				Background(colSelectedBg).
				Foreground(colSelectedFg).
				PaddingLeft(1)

	// Status dots
	dotSuccess = lipgloss.NewStyle().Foreground(colGreen).Render("●")
	dotFailure = lipgloss.NewStyle().Foreground(colRed).Render("●")
	dotRunning = lipgloss.NewStyle().Foreground(colAmber).Render("◉")
	dotNever   = lipgloss.NewStyle().Foreground(colGray).Render("○")

	// Status text
	statusSuccessStyle = lipgloss.NewStyle().Foreground(colGreen)
	statusFailureStyle = lipgloss.NewStyle().Foreground(colRed)
	statusRunningStyle = lipgloss.NewStyle().Foreground(colAmber)

	// Log level prefixes
	logLevelStyle = map[string]lipgloss.Style{
		"info":  lipgloss.NewStyle().Foreground(colLogInfo),
		"INFO":  lipgloss.NewStyle().Foreground(colLogInfo),
		"warn":  lipgloss.NewStyle().Foreground(colLogWarn),
		"WARN":  lipgloss.NewStyle().Foreground(colLogWarn),
		"error": lipgloss.NewStyle().Foreground(colLogError),
		"ERROR": lipgloss.NewStyle().Foreground(colLogError),
		"debug": lipgloss.NewStyle().Foreground(colLogDebug),
		"DEBUG": lipgloss.NewStyle().Foreground(colLogDebug),
	}

	// Footer / help bar
	helpStyle = lipgloss.NewStyle().
			Foreground(colGray).
			Padding(0, 1)

	helpKeyStyle = lipgloss.NewStyle().
			Foreground(colWhite).
			Bold(true)

	// Panel title
	panelTitleStyle = lipgloss.NewStyle().
			Foreground(colPurple).
			Bold(true)

	// Error banner
	errStyle = lipgloss.NewStyle().
			Foreground(colRed).
			Bold(true).
			Padding(0, 1)
)

func statusDot(status string) string {
	switch status {
	case "success":
		return dotSuccess
	case "failure":
		return dotFailure
	case "running":
		return dotRunning
	default:
		return dotNever
	}
}

func levelStyle(level string) lipgloss.Style {
	if s, ok := logLevelStyle[level]; ok {
		return s
	}
	return lipgloss.NewStyle().Foreground(colGray)
}
