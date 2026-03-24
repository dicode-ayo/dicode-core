package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// panel identifies which panel currently has keyboard focus.
type panel int

const (
	panelTasks panel = iota
	panelRuns
	panelLogs
	panelSysLog
)

const maxSysLogLines = 300

// — messages —

type tasksLoadedMsg struct{ tasks []taskItem }
type runsLoadedMsg struct {
	taskID string
	runs   []runItem
}
type logsLoadedMsg struct {
	runID string
	lines []logLine
}
type runStartedMsg struct{ runID string }
type sysLogLineMsg string
type tickMsg time.Time
type errMsg struct{ err error }

// — model —

type model struct {
	// layout
	width, height int
	focused       panel

	// data
	tasks   []taskItem
	runs    []runItem
	logs    []logLine
	sysLogs []string

	// selection indices
	taskCursor int
	runCursor  int

	// state
	loading  bool
	err      error
	showHelp bool

	// components
	logViewport    viewport.Model
	sysLogViewport viewport.Model
	spin           spinner.Model

	// server log channel (filled by SSE goroutine)
	sysLogCh <-chan string

	// client
	api *client
}

func newModel(host string, port int) model {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(colAmber)

	sysLogCh := make(chan string, 256)
	api := newClient(host, port)

	return model{
		api:            api,
		loading:        true,
		spin:           sp,
		logViewport:    viewport.New(0, 0),
		sysLogViewport: viewport.New(0, 0),
		sysLogCh:       sysLogCh,
	}
}

// — init —

func (m model) Init() tea.Cmd {
	// Start SSE subscription — writes to m.sysLogCh from a background goroutine.
	ctx := context.Background()
	ch := make(chan string, 256)
	m.api.StreamServerLogs(ctx, ch)
	// Replace the placeholder channel so waitForSysLog reads from the live one.
	// We can't mutate m here (Init receives a copy), so we pass the channel
	// back as a message via a one-shot command.
	return tea.Batch(
		m.spin.Tick,
		m.fetchTasks(),
		tick(),
		func() tea.Msg { return sysLogChanMsg(ch) },
	)
}

// sysLogChanMsg carries the live channel back into the model on first Update.
type sysLogChanMsg chan string

func tick() tea.Cmd {
	return tea.Tick(5*time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// — update —

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.resizeViewport()

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case tickMsg:
		return m, tea.Batch(m.fetchTasks(), tick())

	case tasksLoadedMsg:
		m.loading = false
		m.err = nil
		m.tasks = msg.tasks
		// Clamp cursor in case tasks changed
		if m.taskCursor >= len(m.tasks) && len(m.tasks) > 0 {
			m.taskCursor = len(m.tasks) - 1
		}
		// Auto-load runs for selected task on first load
		if len(m.tasks) > 0 && len(m.runs) == 0 {
			return m, m.fetchRuns(m.tasks[m.taskCursor].ID)
		}
		return m, nil

	case runsLoadedMsg:
		m.runs = msg.runs
		m.runCursor = 0
		if len(m.runs) > 0 {
			return m, m.fetchLogs(m.runs[0].ID)
		}
		m.logs = nil
		m.updateLogViewport()
		return m, nil

	case logsLoadedMsg:
		m.logs = msg.lines
		m.updateLogViewport()
		// auto-scroll to bottom
		m.logViewport.GotoBottom()
		return m, nil

	case runStartedMsg:
		// Switch to the runs panel and reload runs for current task
		m.focused = panelRuns
		if m.taskCursor < len(m.tasks) {
			return m, m.fetchRuns(m.tasks[m.taskCursor].ID)
		}
		return m, nil

	case sysLogChanMsg:
		m.sysLogCh = msg
		return m, m.waitForSysLog()

	case sysLogLineMsg:
		m.sysLogs = append(m.sysLogs, string(msg))
		if len(m.sysLogs) > maxSysLogLines {
			m.sysLogs = m.sysLogs[len(m.sysLogs)-maxSysLogLines:]
		}
		m.updateSysLogViewport()
		return m, m.waitForSysLog()

	case errMsg:
		m.loading = false
		m.err = msg.err
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	// Forward viewport events when logs panel is focused
	if m.focused == panelLogs {
		var cmd tea.Cmd
		m.logViewport, cmd = m.logViewport.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	key := msg.String()

	// Global keys
	switch key {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "?":
		m.showHelp = !m.showHelp
		return m, nil
	case "tab":
		m.focused = (m.focused + 1) % 4
		return m, nil
	case "esc":
		m.focused = panelTasks
		return m, nil
	case "s":
		return m, m.fetchTasks()
	}

	switch m.focused {
	case panelTasks:
		return m.handleTasksKey(key)
	case panelRuns:
		return m.handleRunsKey(key)
	case panelLogs:
		return m.handleLogsKey(key)
	case panelSysLog:
		return m.handleSysLogKey(key)
	}
	return m, nil
}

func (m model) handleTasksKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "j", "down":
		if m.taskCursor < len(m.tasks)-1 {
			m.taskCursor++
			return m, m.fetchRuns(m.tasks[m.taskCursor].ID)
		}
	case "k", "up":
		if m.taskCursor > 0 {
			m.taskCursor--
			return m, m.fetchRuns(m.tasks[m.taskCursor].ID)
		}
	case "g":
		m.taskCursor = 0
		if len(m.tasks) > 0 {
			return m, m.fetchRuns(m.tasks[0].ID)
		}
	case "G":
		if len(m.tasks) > 0 {
			m.taskCursor = len(m.tasks) - 1
			return m, m.fetchRuns(m.tasks[m.taskCursor].ID)
		}
	case "enter":
		m.focused = panelRuns
		if len(m.tasks) > 0 {
			return m, m.fetchRuns(m.tasks[m.taskCursor].ID)
		}
	case "r":
		if m.taskCursor < len(m.tasks) {
			return m, m.triggerTask(m.tasks[m.taskCursor].ID)
		}
	}
	return m, nil
}

func (m model) handleRunsKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "j", "down":
		if m.runCursor < len(m.runs)-1 {
			m.runCursor++
			return m, m.fetchLogs(m.runs[m.runCursor].ID)
		}
	case "k", "up":
		if m.runCursor > 0 {
			m.runCursor--
			return m, m.fetchLogs(m.runs[m.runCursor].ID)
		}
	case "g":
		m.runCursor = 0
		if len(m.runs) > 0 {
			return m, m.fetchLogs(m.runs[0].ID)
		}
	case "G":
		if len(m.runs) > 0 {
			m.runCursor = len(m.runs) - 1
			return m, m.fetchLogs(m.runs[m.runCursor].ID)
		}
	case "enter":
		m.focused = panelLogs
		if m.runCursor < len(m.runs) {
			return m, m.fetchLogs(m.runs[m.runCursor].ID)
		}
	}
	return m, nil
}

func (m model) handleLogsKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "g":
		m.logViewport.GotoTop()
	case "G":
		m.logViewport.GotoBottom()
	default:
		var cmd tea.Cmd
		m.logViewport, cmd = m.logViewport.Update(tea.KeyMsg(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}))
		return m, cmd
	}
	return m, nil
}

func (m model) handleSysLogKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "g":
		m.sysLogViewport.GotoTop()
	case "G":
		m.sysLogViewport.GotoBottom()
	default:
		var cmd tea.Cmd
		m.sysLogViewport, cmd = m.sysLogViewport.Update(tea.KeyMsg(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}))
		return m, cmd
	}
	return m, nil
}

// — view —

func (m model) View() string {
	if m.width == 0 {
		return "loading..."
	}
	if m.showHelp {
		return m.helpView()
	}

	header := m.headerView()
	footer := m.footerView()

	// available height for all panels
	innerH := m.height - lipgloss.Height(header) - lipgloss.Height(footer)
	if innerH < 6 {
		innerH = 6
	}

	// Split: top area (tasks/runs/logs) takes 65%, syslog takes 35%
	topH := innerH * 65 / 100
	sysH := innerH - topH
	if sysH < 3 {
		sysH = 3
	}

	leftW := m.width * 38 / 100
	rightW := m.width - leftW - 1

	tasksPanel := m.tasksView(leftW, topH)
	rightPanel := m.rightView(rightW, topH)

	top := lipgloss.JoinHorizontal(lipgloss.Top, tasksPanel, rightPanel)
	sysPanel := m.sysLogView(m.width, sysH)

	return lipgloss.JoinVertical(lipgloss.Left, header, top, sysPanel, footer)
}

func (m model) headerView() string {
	host := m.api.base
	taskCount := fmt.Sprintf("%d tasks", len(m.tasks))
	running := m.countRunning()
	runningStr := ""
	if running > 0 {
		runningStr = " " + statusRunningStyle.Render(fmt.Sprintf("◉ %d running", running))
	}
	errStr := ""
	if m.err != nil {
		errStr = " " + errStyle.Render("⚠ "+m.err.Error())
	}
	left := headerStyle.Render("dicode") + dimStyle.Render("  "+host)
	right := dimStyle.Render(taskCount) + runningStr + errStr
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

func (m model) footerView() string {
	var keys []string
	switch m.focused {
	case panelTasks:
		keys = []string{"[j/k]nav", "[enter]select", "[r]run", "[tab]focus", "[s]sync", "[?]help", "[q]quit"}
	case panelRuns:
		keys = []string{"[j/k]nav", "[enter]logs", "[tab]focus", "[esc]tasks", "[q]quit"}
	case panelLogs:
		keys = []string{"[j/k/↑↓]scroll", "[g]top", "[G]bottom", "[tab]focus", "[esc]tasks", "[q]quit"}
	case panelSysLog:
		keys = []string{"[j/k/↑↓]scroll", "[g]top", "[G]tail", "[tab]focus", "[esc]tasks", "[q]quit"}
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = helpStyle.Render(k)
	}
	return strings.Join(parts, dimStyle.Render("·"))
}

func (m model) tasksView(w, h int) string {
	title := panelTitleStyle.Render("Tasks")
	if m.loading {
		title += "  " + m.spin.View()
	}

	innerW := w - 4 // border+padding
	innerH := h - 3 // border+title line
	if innerH < 1 {
		innerH = 1
	}

	var rows []string
	for i, t := range m.tasks {
		lastStatus := m.lastStatus(t.ID)
		dot := statusDot(lastStatus)

		name := t.Name
		if t.Name == "" {
			name = t.ID
		}
		trigger := triggerLabel(t)

		var nameStr string
		if i == m.taskCursor {
			nameStr = taskRowSelectedStyle.Width(innerW).Render(
				dot + " " + taskNameStyle.Render(name),
			)
		} else {
			nameStr = taskRowStyle.Width(innerW).Render(
				dot + " " + name,
			)
		}

		trigStr := taskRowStyle.Width(innerW).Render(
			"  " + taskTriggerStyle.Render(trigger),
		)

		rows = append(rows, nameStr)
		rows = append(rows, trigStr)
		rows = append(rows, "") // spacing
	}

	if len(rows) == 0 {
		rows = []string{dimStyle.Render("  no tasks loaded")}
	}

	// Clamp visible rows to panel height
	content := clampRows(rows, innerH)

	panel := lipgloss.JoinVertical(lipgloss.Left, title, content)

	style := inactiveBorder
	if m.focused == panelTasks {
		style = activeBorder
	}
	return style.Width(w - 2).Height(h - 2).Render(panel)
}

func (m model) rightView(w, h int) string {
	runsH := h * 35 / 100
	logsH := h - runsH

	runsPanel := m.runsView(w, runsH)
	logsPanel := m.logsView(w, logsH)

	return lipgloss.JoinVertical(lipgloss.Left, runsPanel, logsPanel)
}

func (m model) runsView(w, h int) string {
	title := panelTitleStyle.Render("Runs")

	innerW := w - 4
	innerH := h - 3
	if innerH < 1 {
		innerH = 1
	}

	var rows []string
	for i, run := range m.runs {
		dot := statusDot(run.Status)
		dur := runDuration(run)
		age := timeAgo(run.StartedAt)
		ts := run.StartedAt.Format("15:04:05")

		line := fmt.Sprintf("%s  %s  %s  %s", dot, ts, padRight(dur, 8), dimStyle.Render(age))

		var row string
		if i == m.runCursor {
			row = runRowSelectedStyle.Width(innerW).Render(line)
		} else {
			row = runRowStyle.Width(innerW).Render(line)
		}
		rows = append(rows, row)
	}

	if len(rows) == 0 {
		rows = []string{dimStyle.Render("  no runs yet")}
	}

	content := clampRows(rows, innerH)
	panel := lipgloss.JoinVertical(lipgloss.Left, title, content)

	style := inactiveBorder
	if m.focused == panelRuns {
		style = activeBorder
	}
	return style.Width(w - 2).Height(h - 2).Render(panel)
}

func (m model) logsView(w, h int) string {
	title := panelTitleStyle.Render("Logs")

	innerH := h - 4
	if innerH < 1 {
		innerH = 1
	}
	innerW := w - 4

	m.logViewport.Width = innerW
	m.logViewport.Height = innerH

	panel := lipgloss.JoinVertical(lipgloss.Left, title, m.logViewport.View())

	style := inactiveBorder
	if m.focused == panelLogs {
		style = activeBorder
	}
	return style.Width(w - 2).Height(h - 2).Render(panel)
}

func (m model) sysLogView(w, h int) string {
	title := panelTitleStyle.Render("Server Logs")

	innerH := h - 2
	innerW := w - 4
	if innerH < 1 {
		innerH = 1
	}

	m.sysLogViewport.Width = innerW
	m.sysLogViewport.Height = innerH

	panel := lipgloss.JoinVertical(lipgloss.Left, title, m.sysLogViewport.View())

	style := inactiveBorder
	if m.focused == panelSysLog {
		style = activeBorder
	}
	return style.Width(w - 2).Height(h - 2).Render(panel)
}

func (m model) helpView() string {
	help := `
  dicode TUI — keyboard shortcuts

  Navigation
  ──────────────────────────────────────
  j / ↓        Move down
  k / ↑        Move up
  g            Go to top
  G            Go to bottom
  Tab          Cycle focus: Tasks → Runs → Logs
  Esc          Return focus to Tasks
  Enter        Select (task→runs, run→logs)

  Actions
  ──────────────────────────────────────
  r            Trigger selected task manually
  s            Force refresh / sync

  General
  ──────────────────────────────────────
  ?            Toggle this help
  q / Ctrl+C   Quit TUI (server keeps running)
`
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colPurple).
		Padding(1, 3).
		Render(help)
}

// — commands —

func (m model) fetchTasks() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		tasks, err := m.api.listTasks(ctx)
		if err != nil {
			return errMsg{err}
		}
		return tasksLoadedMsg{tasks}
	}
}

func (m model) fetchRuns(taskID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		runs, err := m.api.listRuns(ctx, taskID)
		if err != nil {
			return errMsg{err}
		}
		return runsLoadedMsg{taskID, runs}
	}
}

func (m model) fetchLogs(runID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		lines, err := m.api.getLogs(ctx, runID)
		if err != nil {
			return errMsg{err}
		}
		return logsLoadedMsg{runID, lines}
	}
}

func (m model) waitForSysLog() tea.Cmd {
	return func() tea.Msg {
		line, ok := <-m.sysLogCh
		if !ok {
			return nil
		}
		return sysLogLineMsg(line)
	}
}

func (m model) triggerTask(taskID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		runID, err := m.api.triggerRun(ctx, taskID)
		if err != nil {
			return errMsg{err}
		}
		return runStartedMsg{runID}
	}
}

// — helpers —

func (m *model) resizeViewport() {
	innerH := m.height - 3 // header+footer
	topH := innerH * 65 / 100
	sysH := innerH - topH

	rightW := m.width*62/100 - 4
	if rightW < 10 {
		rightW = 10
	}
	logsH := topH*65/100 - 4
	if logsH < 2 {
		logsH = 2
	}
	m.logViewport.Width = rightW
	m.logViewport.Height = logsH

	sysInnerH := sysH - 2
	if sysInnerH < 1 {
		sysInnerH = 1
	}
	m.sysLogViewport.Width = m.width - 4
	m.sysLogViewport.Height = sysInnerH
}

func (m *model) updateLogViewport() {
	var sb strings.Builder
	for _, l := range m.logs {
		ts := l.Ts.Format("15:04:05")
		lvl := levelStyle(l.Level).Render(fmt.Sprintf("%-5s", l.Level))
		sb.WriteString(fmt.Sprintf("%s %s %s\n",
			dimStyle.Render(ts),
			lvl,
			l.Message,
		))
	}
	m.logViewport.SetContent(sb.String())
}

func (m *model) updateSysLogViewport() {
	var sb strings.Builder
	for _, line := range m.sysLogs {
		// Colour the level token.
		parts := strings.SplitN(line, " ", 3)
		if len(parts) == 3 {
			ts, lvl, msg := parts[0], parts[1], parts[2]
			sb.WriteString(dimStyle.Render(ts) + " " + levelStyle(strings.TrimSpace(lvl)).Render(lvl) + " " + msg + "\n")
		} else {
			sb.WriteString(line + "\n")
		}
	}
	m.sysLogViewport.SetContent(sb.String())
	m.sysLogViewport.GotoBottom()
}

func (m model) lastStatus(taskID string) string {
	for _, r := range m.runs {
		if r.TaskID == taskID {
			return r.Status
		}
	}
	return ""
}

func (m model) countRunning() int {
	n := 0
	for _, r := range m.runs {
		if r.Status == "running" {
			n++
		}
	}
	return n
}

func triggerLabel(t taskItem) string {
	if t.Trigger.Cron != "" {
		return "cron: " + t.Trigger.Cron
	}
	if t.Trigger.Webhook != "" {
		return "webhook: " + t.Trigger.Webhook
	}
	if t.Trigger.Manual {
		return "manual"
	}
	return "—"
}

func runDuration(r runItem) string {
	if r.FinishedAt == nil {
		if r.Status == "running" {
			return statusRunningStyle.Render("running")
		}
		return "—"
	}
	d := r.FinishedAt.Sub(r.StartedAt)
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

func timeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func clampRows(rows []string, maxH int) string {
	if len(rows) > maxH {
		rows = rows[len(rows)-maxH:]
	}
	return strings.Join(rows, "\n")
}

func padRight(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}
