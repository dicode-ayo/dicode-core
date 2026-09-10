# Windowed tasks — design

Addresses [#852](https://github.com/dicode-ayo/dicode-core/issues/852).

[#851](https://github.com/dicode-ayo/dicode-core/issues/851) (the Deno 2.9 bump) is **not** a prerequisite: the deferred `deno desktop` host compiles against its own pinned Deno and never touches the shim or the IPC socket.

dicode tasks are reachable today through the dashboard, a cron schedule, or a webhook. A webhook task can already serve its own HTML UI at `/hooks/<name>` — the dashboard itself is one. What's missing is a way to put that UI in front of you as an ordinary desktop window rather than a browser tab buried among thirty others.

This spec adds a **windowed task**: an ordinary task that takes another task's ID plus window settings, opens a chrome-less native window on that task's own UI, and exits. Users compose windowed apps out of tasks they already have, in params — no new `task.yaml` schema, no Go changes, no per-app build step.

Out of scope: a run-result viewer (pointing a window at a finished run rather than a task UI); a `deno desktop` host; tray or dock integration; Wayland.

Everything below was verified against a running daemon on 2026-09-10 unless marked otherwise.

## User flow

```yaml
# tasks/prs-window/task.yaml
name: "PRs window"
runtime: deno
trigger:
  manual: true
permissions:
  run:
    - google-chrome        # whichever binary `host_binary` resolves to
params:
  target:    { default: "prs" }
  width:     { default: "480" }
  height:    { default: "600" }
  alignment: { default: "center" }
```

Bound to a keybind:

```
bindsym $mod+Shift+g exec --no-startup-id dicode task run prs-window
```

Borderless, translucent and placed because i3 and picom say so, not because dicode does — keyed on the **instance**, see [Window identity](#window-identity):

```
for_window [instance="localhost__hooks_prs"] floating enable, border none, sticky enable
opacity-rule = [ "92:class_i = 'localhost__hooks_prs'" ];
```

## Architecture

The windowed task is a launcher — not a host, not a runner. It resolves a URL, builds an argv, spawns a browser, and returns. Measured: **the launcher returns in ~50 ms**.

```
windowed task (deno run)  ──spawn──▶  google-chrome --app=<url>  ──renders──▶  /hooks/<target>
      │                                       │                                     │
   exits immediately               survives the run                    the target task's own UI
```

Nothing about the window is dicode's to manage after launch. No daemon, no `restart:` policy, no lifecycle — closing the window is a browser event dicode never sees, which is the point.

### Host selection

The host owns the window. Its entire interface is *turn window settings into an argv*, so it is a switch statement, not an abstraction layer. The binary is a config value (`google-chrome`, `chromium`, …), and `permissions.run` names whatever it resolves to.

A `deno desktop` host was evaluated and deliberately deferred. It would add runtime window control, tray and dock integration, `noActivate` popover semantics, and macOS/Windows support. It would also cost a subcommand whose own binary prints `⚠ deno desktop is experimental and subject to change`, a hard runtime dependency on `libwebkit2gtk-4.1` and `libgtk-3` — the dependency [#59](https://github.com/dicode-ayo/dicode-core/issues/59) removed from this binary — a 78 MB artifact per platform, and WebKit as a second rendering target for a UI written against Chrome. Verified against 2.9.6: it also gives *every* window one `WM_CLASS` derived from the binary name, with only the title varying.

Keeping host selection a config value costs one function and buys the option.

### Window identity

**Chrome's `--class`, `--window-position` and `--window-size` are process-startup flags, not per-window flags.** Because every windowed task shares one browser profile ([Authentication](#authentication)), the second and subsequent launches hand off to the already-running browser and **all three flags are silently ignored** — a window launched with `--class=dicode-probe3` came up carrying the first launch's `dicode-webui`.

Two properties *are* per-window, and both are stable:

| Property | Source | Example |
|---|---|---|
| `WM_CLASS` instance (first field) | derived from the URL | `localhost__hooks_auth-providers` |
| `WM_NAME` | the page's own `<title>` | `Auth Providers — dicode` |

So **i3 and picom rules key on instance** (`[instance="…"]`, `class_i`) or on title (`[title="…"]`, `name`) — never on class. The instance is derived from the hook path and needs no cooperation from the target task; the title is the task's to choose.

### Geometry belongs to the window manager

Following from the above, window settings are applied by WM rules rather than browser flags. `alignment: center` maps to i3's native `move position center`; explicit `width`/`height` map to `resize set`. Verified:

```
i3-msg '[instance="localhost__hooks_auth-providers"] floating enable, resize set 600 500, move position center'
→ 594×494 at 666,371 on a 1920×1200 screen
```

The task emits these as a rule set the user pastes into their i3 config, keyed on the instance it will produce. On a desktop that isn't i3, geometry falls back to whatever the browser chooses for the first window.

### Why the target task's own UI

The window renders `/hooks/<target>` — the target's existing webhook UI, served by [pkg/trigger/webhook.go](../../../pkg/trigger/webhook.go)'s task-directory handler, session-gated by `trigger.auth: true`, with design tokens reachable same-origin at `/hooks/webui/app/global.css`.

This means **no new page anywhere**. A purpose-built mini-app view (task name, param form, Run button, log pane) was costed at ~250–300 lines as a webhook task or ~400–550 lines as a Go route, and neither is needed: presentation belongs to the task, where a tiny-app author would look for it. A task that wants a window writes an `index.html`; `window` supplies the frame.

Pointing `window` at a task with no webhook UI must fail with a message naming the target, not open an empty frame.

### Authentication

The window points **straight at `/hooks/<target>`**. The webhook auth guard content-negotiates, so a browser is redirected to the login page on its own and the task needs no knowledge of the auth flow:

```
Accept: text/html  →  303  /login?next=%2Fhooks%2Fwebui
no Accept          →  401  {"error":"unauthorized"}
```

Log in once with *trust this browser* ticked and every later window opens silently — verified: a second window pointed at `/hooks/webui` loaded the SPA with no login. `hasValidSession` ([pkg/webui/auth.go](../../../pkg/webui/auth.go)) renews straight from the `dicode_device` cookie with no passphrase, and the token rotates on each renewal with expiry pushed out another 30 days, so windows in regular use never ask again. `server.device_binding` defaults to `off`; even under `strict` a local window is a stable UA family from localhost and will not drift.

**All windowed tasks share one Chromium profile directory** under the dicode cache. The device cookie lives in that jar, so per-task profiles would mean a login per app. A shared profile also keeps windowed tasks clear of the user's everyday browser profile, and makes a second window open under the already-running browser process — which is what makes the ~50 ms launcher return possible.

The cost is [Window identity](#window-identity): the shared process is exactly why the class and geometry flags stop working. Per-task profiles would restore them at the price of one login per app per 30 days; that trade was considered and declined.

A one-shot session-bootstrap token (copying the single-use, TTL'd, hashed `/approve/{token}` machinery in [pkg/approval/token.go](../../../pkg/approval/token.go)) was **rejected for the Chromium host**: the URL *is* argv, and argv is world-readable through `/proc/<pid>/cmdline`. [pkg/onboarding/browser.go](../../../pkg/onboarding/browser.go) already refused that exact trade when it chose to print the onboarding PIN to the terminal. The token route becomes available if a `deno desktop` host ever lands, since that receives its URL over a pipe.

### Detached stdio is mandatory

The host **must** be spawned with `stdout`, `stderr` and `stdin` all set to `"null"`.

The Deno runtime reads task output through `cmd.StdoutPipe()` and does not return from `Run` until the log scanners see EOF ([pkg/runtime/deno/runtime.go](../../../pkg/runtime/deno/runtime.go), the `wg.Wait()` guarding log flush). A spawned browser that inherits that pipe holds its write end open for as long as the window is open, so the scanner never gets EOF and **the run hangs indefinitely** — while the window works and everything appears fine. Every launch would leak a stuck run.

Nothing kills the browser when the run ends: there is no `Setpgid` and no process-group kill, and termination signals only the Deno process itself. That is what makes the launcher model work, and it is also why the pipe must be detached explicitly rather than left to cleanup.

## New files

- `tasks/window/` in [dicode-buildin](https://github.com/dicode-ayo/dicode-buildin) — `task.yaml`, `task.ts`, `task.test.ts`. The whole task is: resolve the target's hook URL, reject a target with no UI, build the host argv, spawn detached, exit — plus emitting the WM rule set for the instance it will produce.

## Modified files

None in dicode-core.

## Testing

- Argv construction per host, from a table of window settings — the only logic worth unit-testing.
- Instance derivation from a hook path, since every WM rule the feature emits depends on it.
- The no-UI target case: a clear error naming the target, not a spawn.
- A regression test asserting stdio is detached, since the failure mode is a hung run rather than a visible error and would otherwise be found in production.

## Migration / rollout

Additive. No existing task changes behavior; nothing is removed. Windowed tasks are user-authored and opt-in.

Headless installs degrade by not having any: a windowed task on a machine with no display fails at spawn with the browser's own error, which is the correct outcome and needs no `server.tray`-style switch.

## Follow-ups (tracked separately, out of scope here)

- **The login page is unusable in a narrow window.** Its heading is the target task's full `description`, which in a 520 px frame is eleven lines of prose pushing the password field off the fold. This affects every windowed task's first launch and is a dicode-core defect, not something a windowed task can work around.
- **`deno desktop` host**, if tray integration or true popover semantics are ever wanted. Gated on the subcommand leaving experimental.
- **Run-result mode**: point a window at `/runs/<id>/result` (already a bare, chrome-less page) instead of a task UI. A run viewer is genuinely useful but a separate product.
- **Wayland**: none of the i3/picom half of this applies there.
