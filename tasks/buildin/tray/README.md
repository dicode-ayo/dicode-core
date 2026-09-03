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

- **Most X11 bars speak only the legacy XEmbed protocol, not SNI** — i3bar and
  polybar included. So an SNI icon won't show even with i3bar's `tray_output`
  or polybar's `tray-position` enabled.
- There's no error — the item registers into the void.

The task logs a one-line hint at startup when it detects no SNI host (see
"Startup hint" below).

### Fixes

Every fix is the same shape: run something that registers as a
StatusNotifierHost. Which one depends on your bar.

- **waybar** (Wayland) hosts SNI itself through its `tray` module — nothing
  extra to install.
- **X11 bars host only XEmbed**, so they need `snixembed` in front. It presents
  itself as an SNI host and maintains a matching XEmbed icon for each item it
  sees, which the bar's own tray then renders:
  ```
  # ~/.config/i3/config — must precede the SNI apps, which look for a host at
  # their own startup
  exec --no-startup-id snixembed --fork
  ```
  Then keep your bar's existing tray: polybar's `tray-position` (or
  `[module/tray]` on >=3.7), or `tray_output primary` in i3bar's `bar { }`
  block. Polybar has **no** SNI support at any version — both of its tray
  forms are XEmbed.

  Use `--fork` rather than `&`: it forks once the host is on the bus, so apps
  that publish an icon only when a host already exists don't race it.

`snixembed` is packaged on Arch (AUR) and Debian trixie+, but **not on any
Ubuntu LTS**. Build it from source there:

```
sudo apt install -y make valac libgtk-3-dev libglib2.0-dev \
                    libdbusmenu-gtk3-dev libdbusmenu-glib-dev
git clone https://git.sr.ht/~steef/snixembed && cd snixembed
make && sudo make install     # -> /usr/bin/snixembed
```

KDE's `xembedsniproxy` is **not** an alternative. It bridges XEmbed -> SNI, the
opposite direction, so that legacy tray apps appear under Plasma.

Not every item re-registers when a host shows up later, and this task's helper
is one that doesn't. After starting a host for the first time, restart the task
— `restart: always` brings it straight back:

```
pkill -f buildin/tray
```

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
