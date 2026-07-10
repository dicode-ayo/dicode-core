# buildin/tray — system tray icon

A daemon task that shows dicode's status-bar icon with **Open Dashboard** and
**Quit dicode** menu items. It drives a pre-compiled native helper
(`systray-portable`) over stdin/stdout — no CGO, no GTK, no build dependency on
the dicode binary (replaces the old `pkg/tray/` Go package; see
[#59](https://github.com/dicode-ayo/dicode-core/issues/59)).

Runtime: Deno. Trigger: `daemon: true`, `restart: always` — dicoded keeps it
alive and relaunches it if the helper crashes. The dashboard port comes from
`DICODE_PORT` (falls back to `8080`).

## The icon doesn't show up (i3, dwm, bspwm, sway…)

On Linux the tray uses the **StatusNotifierItem (SNI / AppIndicator)** DBus
protocol. The icon only appears if a **StatusNotifierHost** — a compatible bar
or tray — is running on your session bus. Full desktops (GNOME, KDE, XFCE)
ship one; **bare window managers don't**, so the icon is silently invisible
even though the task is running fine.

Two things confuse people here:

- **i3bar only supports the legacy XEmbed protocol, not SNI.** So even with
  i3bar's `tray_output` enabled, an SNI icon won't show.
- There's no error — the item registers into the void.

The task logs a one-line hint at startup when it detects no SNI host (see
"Startup hint" below).

### Fixes

- **polybar ≥ 3.6** has a native SNI tray. Add a tray module and use polybar as
  your bar:
  ```ini
  [module/tray]
  type = internal/tray
  tray-size = 66%
  ```
- **waybar** (Wayland) supports SNI via its `tray` module.
- **Stay on i3bar** by running an SNI→XEmbed bridge, then i3bar renders it:
  ```
  # ~/.config/i3/config
  exec --no-startup-id snixembed        # or: xembedsniproxy (from KDE)
  # in the bar { } block:
  tray_output primary
  ```
  On Debian/Ubuntu: `sudo apt install snixembed`.

macOS and Windows render the tray natively and are unaffected.

## Startup hint

At startup (on Linux, after a short grace period for the bar to come up) the
task probes the session bus via `gdbus` for a registered SNI host
(`IsStatusNotifierHostRegistered`). If none is found it logs an actionable hint
pointing here. The probe is best-effort: if `gdbus` isn't available or the
reply can't be read, it stays quiet rather than warning on a guess. See
`sni.ts`.

## Tests

```
make test-tasks
# or just this task:
deno test --allow-all --config=tasks/deno.json tasks/buildin/tray/task.test.ts
```
