// sni.ts — detect whether a Linux system-tray host will actually display the
// icon.
//
// Linux tray icons use the StatusNotifierItem (SNI) DBus protocol. The icon is
// only shown if a StatusNotifierHost (a compatible bar/tray) is registered on
// the session bus. On bare window managers (i3/dwm/bspwm/sway) none runs by
// default, so the icon is silently invisible with no error. macOS (Cocoa) and
// Windows (Win32) render the tray natively and never hit this path.

const WATCHERS = [
  "org.kde.StatusNotifierWatcher",
  "org.freedesktop.StatusNotifierWatcher",
];

/** Interpret a `gdbus call … IsStatusNotifierHostRegistered` reply
 *  (e.g. `(<true>,)`). Returns null when the reply can't be interpreted. */
export function parseHostRegistered(stdout: string): boolean | null {
  if (/\btrue\b/.test(stdout)) return true;
  if (/\bfalse\b/.test(stdout)) return false;
  return null;
}

/** Query one watcher's IsStatusNotifierHostRegistered via gdbus. Returns the
 *  raw stdout, or null if that watcher name is not registered. */
async function gdbusRunner(dest: string): Promise<string | null> {
  const { code, stdout } = await new Deno.Command("gdbus", {
    args: [
      "call", "--session", "--dest", dest,
      "--object-path", "/StatusNotifierWatcher",
      "--method", "org.freedesktop.DBus.Properties.Get",
      dest, "IsStatusNotifierHostRegistered",
    ],
    stdout: "piped",
    stderr: "null",
  }).output();
  return code === 0 ? new TextDecoder().decode(stdout) : null;
}

/** Best-effort: is an SNI host registered on the session bus?
 *  - true/false when a watcher answers definitively,
 *  - false when no watcher exists at all (a host is then impossible),
 *  - null when we can't tell (gdbus missing/erroring, or an unreadable reply),
 *    so callers can stay quiet instead of warning on a guess. */
export async function probeSNIHost(
  runner: (dest: string) => Promise<string | null> = gdbusRunner,
): Promise<boolean | null> {
  let sawWatcher = false;
  for (const dest of WATCHERS) {
    let out: string | null;
    try {
      out = await runner(dest);
    } catch {
      return null; // tooling unusable — don't nag
    }
    if (out === null) continue; // this watcher name absent, try the next
    sawWatcher = true;
    const registered = parseHostRegistered(out);
    if (registered !== null) return registered;
  }
  return sawWatcher ? null : false;
}

/** An actionable hint when the tray icon will likely be invisible, else null.
 *  Pure and synchronous so the decision is unit-testable. */
export function trayVisibilityHint(
  os: string,
  hostRegistered: boolean | null,
): string | null {
  if (os !== "linux") return null; // macOS/Windows render natively
  if (hostRegistered !== false) return null; // true = shown; null = unknown
  return (
    "tray: no StatusNotifierItem host is running on the session bus, so the " +
    "icon will not appear. Bare window managers (i3/dwm/bspwm/sway) don't host " +
    "SNI by default. On Wayland use waybar's tray module; on X11 run " +
    "`snixembed --fork` in front of your bar — i3bar and polybar render " +
    "only XEmbed. " +
    "See tasks/buildin/tray/README.md."
  );
}

/** Probe the bus and, on Linux with no host, log a single actionable hint. */
export async function warnIfTrayInvisible(
  log: (msg: string) => void = console.warn,
): Promise<void> {
  if (Deno.build.os !== "linux") return;
  const hint = trayVisibilityHint("linux", await probeSNIHost());
  if (hint) log(hint);
}
