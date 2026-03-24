# dicode TUI — Design Document

**Last updated:** 2026-03-24
**Status:** Approved for implementation

---

## 1. Goals

Add a terminal user interface (TUI) so users running dicode on a server or in a terminal-first workflow can monitor, inspect, and control tasks without opening a browser. The TUI is a **read-mostly client** that connects to a running dicode server over HTTP — it calls the same REST API as the web UI.

**In scope for v1:**
- Task list with live status indicators
- Run history per task (last 50)
- Log viewer per run with scroll
- Manual trigger of any task
- Keyboard-driven navigation
- Auto-refresh (5s poll + SSE for active runs)

**Out of scope for v1 (north star):**
- AI chat panel
- Task editor
- Secret management
- Config editing

---

## 2. Library Choices

| Concern | Library | Why |
|---|---|---|
| TUI framework | `github.com/charmbracelet/bubbletea` | Elm-architecture, most maintained Go TUI lib, excellent community |
| Styling | `github.com/charmbracelet/lipgloss` | CSS-like, composable, pairs perfectly with bubbletea |
| Components | `github.com/charmbracelet/bubbles` | Viewport (scrollable text), spinner, text input — no reimplementation |
| Color depth | Auto via lipgloss | Uses `termenv` to detect 256-color / TrueColor / NoColor |

All three are MIT licensed. No CGo, no system dependencies.

---

## 3. Layout

```
┌─ dicode ── localhost:8080 ───────────────────── 3 tasks ─ [q]quit ─┐
│                                                                      │
│  ┌─ Tasks ──────────────────────────┐  ┌─ Runs ─────────────────┐  │
│  │                                  │  │                         │  │
│  │ ● morning-check           ✓ 2m  │  │ ✓  12:34  23ms          │  │
│  │   cron: 0 9 * * *               │  │ ✓  09:00  18ms  1d ago  │  │
│  │                                  │  │ ✗  09:00  FAILED 2d ago │  │
│  │ ● api-health-check        ✓ 1m  │  │ ✓  09:00  12ms  3d ago  │  │
│  │   cron: */5 * * * *             │  │                         │  │
│  │                                  │  └─────────────────────────┘  │
│  │ ◉ weekly-report         ◉ now  │                               │
│  │   cron: 0 9 * * 1               │  ┌─ Logs ─────────────────┐  │
│  │                                  │  │                         │  │
│  │                                  │  │ 12:34:01 INFO starting  │  │
│  │                                  │  │ 12:34:02 INFO found 3   │  │
│  │                                  │  │ 12:34:03 INFO sent ✓    │  │
│  │                                  │  │                         │  │
│  └──────────────────────────────────┘  └─────────────────────────┘  │
│                                                                      │
│  [r]un  [j/k↑↓]nav  [tab]focus  [s]sync  [esc]back  [?]help [q]quit │
└──────────────────────────────────────────────────────────────────────┘
```

### Panels

| Panel | Position | Content |
|---|---|---|
| **Tasks** | Left (38% width) | All registered tasks, sorted by last run desc |
| **Runs** | Right-top (62% width, 35% height) | Last 50 runs for the selected task |
| **Logs** | Right-bottom (62% width, 65% height) | Log lines for the selected run |

### Status Indicators

| Symbol | Color | Meaning |
|---|---|---|
| `●` | Green `#22c55e` | Last run succeeded |
| `●` | Red `#ef4444` | Last run failed |
| `◉` | Yellow `#f59e0b` | Currently running |
| `○` | Gray `#6b7280` | Never run |

---

## 4. Navigation Model

Three panels; `Tab` cycles focus. Focused panel has a **purple** border (`#7c3aed`), unfocused panels use **dim gray**.

### Key bindings

| Key | Context | Action |
|---|---|---|
| `j` / `↓` | Tasks focused | Move cursor down |
| `k` / `↑` | Tasks focused | Move cursor up |
| `j` / `↓` | Runs focused | Move cursor down |
| `k` / `↑` | Runs focused | Move cursor up |
| `j` / `↓` | Logs focused | Scroll down |
| `k` / `↑` | Logs focused | Scroll up |
| `g` | Any | Jump to top |
| `G` | Any | Jump to bottom (Logs: tail mode) |
| `Tab` | Any | Cycle focus: Tasks → Runs → Logs → Tasks |
| `Enter` | Tasks | Select task → load runs + latest logs |
| `Enter` | Runs | Select run → load logs |
| `r` | Tasks | Trigger selected task manually |
| `s` | Any | Force sync (POST /api/sync) |
| `?` | Any | Toggle help overlay |
| `Esc` | Any | Return focus to Tasks panel |
| `q` | Any | Quit TUI (dicode server keeps running) |

---

## 5. Color Scheme

```
Background:       #0f172a  (near black)
Panel border:     #374151  (inactive)  /  #7c3aed  (active, purple)
Header text:      #f9fafb  (white)
Dim text:         #6b7280  (gray)
Success:          #22c55e  (green)
Failure:          #ef4444  (red)
Running:          #f59e0b  (yellow/amber)
Cron hint:        #818cf8  (indigo-ish)
Selected row bg:  #1e1b4b  (dark purple)
Selected row fg:  #e0e7ff  (light purple)
Log INFO:         #94a3b8  (slate)
Log WARN:         #fbbf24  (amber)
Log ERROR:        #f87171  (rose)
Log DEBUG:        #64748b  (slate dim)
```

Degrades gracefully to 256-color and then to 8-color via lipgloss's `AdaptiveColor`.

---

## 6. Data Flow

```
dicode tui --port 8080
    │
    ├── Init
    │   ├── GET /api/tasks        → populate task list
    │   └── select first task
    │       └── GET /api/tasks/{id}/runs   → populate runs panel
    │           └── GET /api/runs/{id}/logs → populate logs panel
    │
    ├── Every 5s tick
    │   └── GET /api/tasks        → refresh statuses (running/idle/error)
    │
    ├── On task select (Enter)
    │   ├── GET /api/tasks/{id}/runs
    │   └── GET /api/runs/{latest}/logs
    │
    ├── On run select (Enter)
    │   └── GET /api/runs/{id}/logs
    │
    └── On task trigger (r)
        └── POST /api/tasks/{id}/run  → returns runId
            └── poll /api/runs/{runId}/logs every 500ms until status != running
```

SSE (`/logs/stream`) is the system-level log stream (zap output). The TUI instead polls the per-run structured log endpoint for run-specific logs — this gives cleaner, task-scoped output.

---

## 7. Entry Point

```bash
dicode tui                  # connect to http://localhost:8080
dicode tui --port 9090      # different port
dicode tui --host 192.168.1.5 --port 8080  # remote instance
```

The TUI is entirely read-capable without auth. If the server has `server.secret` set, the TUI will need to pass it via `--token` flag (north star).

---

## 8. Package Structure

```
pkg/tui/
├── tui.go        — Run(port, host) entry point
├── model.go      — bubbletea Model, Init/Update/View
├── styles.go     — all lipgloss styles
└── client.go     — HTTP client wrapping the REST API
```

---

## 9. Dependencies to Add

```
github.com/charmbracelet/bubbletea  v1.x
github.com/charmbracelet/lipgloss   v1.x
github.com/charmbracelet/bubbles    v0.x
```

All pure Go, no CGo, no system libraries.

---

## 10. North Star (v2+)

- SSE streaming for active run logs (instead of polling)
- Inline task trigger with param input (text input from bubbles)
- Filter/search task list
- Log search / highlight
- AI chat panel (right side, collapsible)
- Multiple selected tasks for bulk trigger
- Mouse support (bubbletea has native mouse event support)
