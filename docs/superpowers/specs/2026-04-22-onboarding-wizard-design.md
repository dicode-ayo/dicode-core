# Onboarding wizard — design

Addresses [#85](https://github.com/dicode-ayo/issues/85). Alpha epic [#82](https://github.com/dicode-ayo/dicode-core/issues/82) Lane 3.

Today [pkg/onboarding/onboarding.go](../../../pkg/onboarding/onboarding.go) detects first run and silently writes a local-only `dicode.yaml`. The site promises a guided first-run experience; this spec delivers it: a wizard that runs before the daemon starts, offers browser or CLI, picks curated tasksets, generates a dashboard passphrase, and writes a ready-to-run config.

Out of scope: `dicode relay login` handoff (Lane 3 item #89/#90, blocked on relay-side work). Out of scope: starter-task seeding, custom YAML diff preview.

## User flow

```
$ dicode daemon
Welcome to dicode. No config found.
Set up in [b]rowser or [c]li? [b]
```

- TTY + display → prompt; default **browser**
- TTY + no display (SSH, headless) → auto **CLI**
- Non-TTY (systemd, Docker, CI) → **silent default** (same as today, but wired to all three curated tasksets enabled)

### Questions (same in both surfaces, same order)

1. **Curated tasksets** — three checkboxes, all default on:
   - ☑ Built-in (tray, notifications, webui, alert, dicodai…)
   - ☑ Examples (hello-cron, github-stars, webhook-form, nginx-start…)
   - ☑ OAuth providers (Google, GitHub, Slack, OpenRouter…)

   Any combination allowed, including none.

2. **Local tasks directory** — text input for user-authored tasks. Default `~/dicode-tasks`. "Skip" option allowed (omits the `type: local` entry).

3. **Advanced** (collapsed/hidden by default):
   - Data directory — default `~/.dicode`
   - Daemon HTTP port — default `8080`

4. **Review** — shows the generated YAML plus a clearly-flagged dashboard passphrase that the user must copy now.

### On apply

- Write `dicode.yaml` to the resolved config path
- Write `pkg/onboarding/presets.go`-derived git sources for every selected preset
- Set `server.auth: true` + `server.secret: "<random>"` in YAML
- Print to the daemon's stdout:

  ```
  ━━━ dicode setup complete ━━━
  Config written to /path/to/dicode.yaml
  Dashboard: http://localhost:8080
  Login passphrase (copy now, shown once):

      Xk9mN2pQ7vR4tY8uI3oP6wE1

  Store this somewhere safe. To change it later: edit server.secret in
  your dicode.yaml and restart, or use the web UI under Settings.
  ━━━━━━━━━━━━━━━━━━━━━━━━━━━
  ```
- Browser path also renders the same block on the final wizard page before the listener shuts down
- Daemon proceeds normally in the same process

## Architecture

```
cmd/dicoded/main.go
   │
   └── daemon.Run(configPath)
          │
          └── if onboarding.Required(configPath):
                 │
                 └── onboarding.Run(ctx, configPath) ──► writes dicode.yaml
                        │
                        ├── pick surface (browser | cli | silent)
                        ├── Result{TaskSets, LocalDir, DataDir, Port, Passphrase}
                        ├── render yaml via DefaultConfig(Result)
                        └── print success block to stdout
```

### New files

| File | Role |
|---|---|
| `pkg/onboarding/presets.go` | Hardcoded table of curated tasksets. Single edit point when repos split out. |
| `pkg/onboarding/surface.go` | `PickSurface(stdin, stdout, env)` — returns `browser` / `cli` / `silent` based on TTY, DISPLAY, explicit flag. |
| `pkg/onboarding/cli.go` | `RunCLI(stdin, stdout) (Result, error)` — bufio-based prompts. |
| `pkg/onboarding/browser.go` | `RunBrowser(ctx) (Result, error)` — ephemeral HTTP server + embedded HTML/JS form. |
| `pkg/onboarding/render.go` | `RenderConfig(Result) string` — generates the YAML. Replaces the current `DefaultLocalConfig`. |
| `pkg/onboarding/passphrase.go` | `GeneratePassphrase() string` — 18 random bytes → base64url → 24 chars. |
| `pkg/onboarding/onboarding.go` | Existing file kept; add `Run(ctx, configPath) error` as the single entry point called from daemon. |

### Modified files

- `pkg/daemon/daemon.go` — replace the current silent default at lines 48–60 with `onboarding.Run(ctx, configPath)`. No signature change to `daemon.Run()`.

### Preset table

```go
// pkg/onboarding/presets.go
package onboarding

type TaskSetPreset struct {
    Name      string // unique namespace segment
    Label     string // shown in UI
    Desc      string // one-line description for the UI
    URL       string // git URL
    Branch    string // default "main"
    EntryPath string // path in repo to taskset.yaml
    DefaultOn bool
}

// Single edit point when tasksets split into standalone repos.
var TaskSetPresets = []TaskSetPreset{
    {
        Name:      "buildin",
        Label:     "Built-in tasks",
        Desc:      "Tray icon, notifications, web UI, dicodai chat, alert — the daemon's standard inventory.",
        URL:       "https://github.com/dicode-ayo/dicode-core",
        Branch:    "main",
        EntryPath: "tasks/buildin/taskset.yaml",
        DefaultOn: true,
    },
    {
        Name:      "examples",
        Label:     "Examples",
        Desc:      "Copy-friendly samples: hello-cron, github-stars, webhook-form, nginx-start, and more.",
        URL:       "https://github.com/dicode-ayo/dicode-core",
        Branch:    "main",
        EntryPath: "tasks/examples/taskset.yaml",
        DefaultOn: true,
    },
    {
        Name:      "auth",
        Label:     "OAuth providers",
        Desc:      "Zero-paste OAuth for Google, GitHub, Slack, OpenRouter, Spotify, and more.",
        URL:       "https://github.com/dicode-ayo/dicode-core",
        Branch:    "main",
        EntryPath: "tasks/auth/taskset.yaml",
        DefaultOn: true,
    },
}
```

### Surface selection

```go
// pkg/onboarding/surface.go
type Surface int

const (
    SurfaceBrowser Surface = iota
    SurfaceCLI
    SurfaceSilent
)

// PickSurface resolves which wizard surface to run.
//   - non-TTY stdin → Silent (systemd, Docker, CI)
//   - TTY without display env → CLI
//   - TTY with display → ask; default Browser
//
// Explicit override: DICODE_ONBOARDING env var = "browser" | "cli" | "silent"
// bypasses detection.
func PickSurface(in io.Reader, out io.Writer, env func(string) string) (Surface, error)
```

TTY detection: `golang.org/x/term.IsTerminal(os.Stdin.Fd())`. Display detection on Linux: either `DISPLAY` or `WAYLAND_DISPLAY` set. On darwin/windows: assume display is available when TTY is.

### Browser wizard

Loopback is **not** a per-user trust boundary on Linux — `/proc/net/tcp` leaks any listener to any local user. The wizard defends against a local-race-to-submit attack with a single-use token:

- `RunBrowser` generates a random 128-bit hex token, embeds it in the URL (`http://127.0.0.1:<port>/?t=<token>`), and opens the browser to that URL.
- The embedded JS reads `?t=` from `window.location.search` and sends it back as `X-Setup-Token` on the POST to `/setup/apply`.
- The handler constant-time-compares `X-Setup-Token` against the token; mismatch → 403.

Additionally, `/setup/apply` calls the caller-supplied `apply(Result)` function **before** returning the passphrase, so the client never sees a credential the daemon hasn't stored. If `apply` fails → 500, no passphrase in response, nothing on the channel.

```go
// pkg/onboarding/browser.go
func RunBrowser(ctx context.Context, home string, apply func(Result) error) (Result, error) {
    ln, err := net.Listen("tcp", "127.0.0.1:0")  // random port
    if err != nil { return Result{}, err }
    defer ln.Close()

    resultCh := make(chan Result, 1)
    mux := http.NewServeMux()
    mux.HandleFunc("/", serveWizard)         // GET /  → embedded index.html
    mux.HandleFunc("/setup/apply", ...)      // POST   → resultCh <- form
    mux.HandleFunc("/setup/done", ...)       // GET    → success + passphrase

    srv := &http.Server{Handler: mux}
    go srv.Serve(ln)

    url := "http://" + ln.Addr().String()
    fmt.Println("Open your browser to", url)
    _ = openBrowser(url)  // best effort; no-op on failure

    select {
    case r := <-resultCh:
        // Client has loaded /setup/done and seen the passphrase.
        // Give them 60s to read, then shut down gracefully.
        go func() {
            time.Sleep(60 * time.Second)
            srv.Shutdown(context.Background())
        }()
        return r, nil
    case <-ctx.Done():
        srv.Shutdown(context.Background())
        return Result{}, ctx.Err()
    }
}
```

Static assets: single `index.html` + one vanilla-JS file, both embedded via `//go:embed`. No build step, no framework. Form posts JSON to `/setup/apply`.

`openBrowser` — `xdg-open` / `open` / `cmd /c start` per GOOS; ignore failure (user reads the URL from stdout).

### CLI wizard

```go
// pkg/onboarding/cli.go
func RunCLI(in io.Reader, out io.Writer) (Result, error) {
    scanner := bufio.NewScanner(in)
    res := Result{}

    // Q1: tasksets — loop through TaskSetPresets, ask y/n for each
    // Q2: local tasks dir — text, blank → skip
    // Q3: advanced (data dir, port) — single "edit advanced? [y/N]" gate
    res.Passphrase = GeneratePassphrase()
    return res, nil
}
```

### YAML rendering

```go
// pkg/onboarding/render.go
type Result struct {
    TaskSetsEnabled map[string]bool  // keyed by TaskSetPreset.Name
    LocalTasksDir   string           // "" → no local source
    DataDir         string
    Port            int
    Passphrase      string
}

func RenderConfig(r Result) string
```

Emits a commented YAML file whose structure matches the existing `DefaultLocalConfig` *plus* a `sources:` array assembled from preset selections and optional local dir *plus* `server.auth: true` and `server.secret: "<passphrase>"`.

### Passphrase generation

```go
// pkg/onboarding/passphrase.go
import (
    "crypto/rand"
    "encoding/base64"
)

// GeneratePassphrase returns a 24-character URL-safe base64 token
// (18 random bytes → ~108 bits of entropy).
func GeneratePassphrase() string {
    var b [18]byte
    if _, err := rand.Read(b[:]); err != nil {
        panic(err) // crypto/rand never fails on modern kernels
    }
    return base64.RawURLEncoding.EncodeToString(b[:])
}
```

Panics on `rand.Read` failure — that only happens on truly broken systems where the daemon wouldn't work anyway. Matches the convention in `pkg/relay/keys.go`.

### Final stdout block

```go
// pkg/onboarding/render.go
func PrintSuccess(out io.Writer, r Result, configPath string) {
    fmt.Fprintln(out, "━━━ dicode setup complete ━━━")
    fmt.Fprintln(out, "Config written to", configPath)
    fmt.Fprintf(out, "Dashboard: http://localhost:%d\n", r.Port)
    fmt.Fprintln(out, "Login passphrase (copy now, shown once):")
    fmt.Fprintln(out, "")
    fmt.Fprintln(out, "    "+r.Passphrase)
    fmt.Fprintln(out, "")
    fmt.Fprintln(out, "Store this somewhere safe. To change later: edit")
    fmt.Fprintln(out, "server.secret in your dicode.yaml and restart.")
    fmt.Fprintln(out, "━━━━━━━━━━━━━━━━━━━━━━━━━━━")
}
```

Called after the YAML is written on *both* surfaces. Browser surface additionally renders it inside the wizard's "Setup complete" page, with a copy button.

## Testing

| Test | File | Asserts |
|---|---|---|
| Preset shape valid | `presets_test.go` | each has non-empty name/label/url/entry_path; names unique |
| `GeneratePassphrase` entropy | `passphrase_test.go` | 24 chars, URL-safe, 100 calls produce 100 distinct values |
| `RenderConfig` — all tasksets enabled | `render_test.go` | YAML parses via `yaml.Unmarshal`; `sources` has 3 git + 1 local; `server.auth == true`; `server.secret` matches passphrase |
| `RenderConfig` — partial selection | `render_test.go` | only selected presets appear in `sources` |
| `RenderConfig` — skip local dir | `render_test.go` | no `type: local` entry when LocalTasksDir is empty |
| `PickSurface` — non-TTY stdin | `surface_test.go` | returns `SurfaceSilent` |
| `PickSurface` — TTY no display | `surface_test.go` | returns `SurfaceCLI` (env map {DISPLAY: ""}) |
| `PickSurface` — TTY + display | `surface_test.go` | returns `SurfaceBrowser` |
| `PickSurface` — env override | `surface_test.go` | `DICODE_ONBOARDING=cli` overrides detection |
| `RunCLI` happy path | `cli_test.go` | feed scripted stdin, assert Result fields |
| `RunCLI` skip local dir | `cli_test.go` | empty-line response → `LocalTasksDir == ""` |
| `RunBrowser` — POST `/setup/apply` | `browser_test.go` | httptest request with JSON body yields matching Result |
| `RunBrowser` — lifecycle | `browser_test.go` | server shuts down after context cancel |
| Daemon integration — no config → wizard → daemon starts | `daemon_test.go` (new small fixture) | stubbed `PickSurface`=silent; config appears on disk; daemon completes init |

No change to existing tests.

## Migration / rollout

- Existing installs with a `dicode.yaml` — unaffected (wizard only runs when config is missing).
- Fresh installs on systemd/Docker — same silent-default behavior as today, but YAML now includes the three curated git sources enabled by default, so users see example tasks immediately.
- No schema changes; the wizard only fills values for fields `pkg/config` already knows about.

## Follow-ups (tracked separately, out of scope here)

- Relay opt-in step → depends on dicode-relay#14/#15 landing; wire via `dicode relay login` handoff post-alpha.
- Starter template: seed a `hello.yaml` in the chosen local tasks dir — small, but bundles example content and deserves its own review pass.
- When buildins/examples/auth split into standalone repos, flip the URLs in `presets.go` in a one-line PR.
