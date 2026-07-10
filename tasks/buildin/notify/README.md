# buildin/notify — native desktop notification

Delivers a native OS desktop notification. Manual trigger; params: `title`
(required), `body` (required), `priority` (`min|low|default|high|urgent`),
`tags` (comma-separated emoji shortcodes), `icon` (Linux freedesktop icon
name). Replaces the browser Service Worker notification system
([#60](https://github.com/dicode-ayo/dicode-core/issues/60)).

Delivery per platform:

- **Linux** — `notify-send` (libnotify). Install with `apt install libnotify-bin`
  / `pacman -S libnotify`.
- **macOS** — `osascript display notification`.
- **Windows** — PowerShell `NotifyIcon.ShowBalloonTip`.

## Nothing appears on Linux (i3, dwm, sway…)

`notify-send` only hands the message to the **`org.freedesktop.Notifications`**
DBus interface — it does not draw anything itself. A **notification daemon**
must be running to display it. Full desktops (GNOME, KDE, XFCE) ship one, but
**bare window managers don't**, so `notify-send` can be installed and still show
nothing. When no daemon is running (and none is D-Bus-activatable), the call
fails with a GDBus `ServiceUnknown` error; the task rewrites that into an
actionable message (see `failure.ts`) instead of surfacing the raw stack.

### Fix

Run a notification daemon:

- **dunst** — the common choice on X11 / i3. `sudo apt install dunst`, then start
  it from your WM (`exec --no-startup-id dunst`).
- **mako** — for Wayland (sway).

Full desktop environments need no extra setup.

## Tests

```
make test-tasks
# or just this task:
deno test --allow-all --config=tasks/deno.json tasks/buildin/notify/task.test.ts
```
