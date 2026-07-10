// failure.ts — turn a failed notification command into an actionable error.
//
// On Linux, notify-send delivers over the org.freedesktop.Notifications DBus
// interface. The `notify-send` binary can be installed and still deliver
// nothing if no notification daemon is running (and none is D-Bus-activatable)
// — the default on bare window managers (i3/dwm/bspwm/sway). In that case the
// call fails with a GDBus ServiceUnknown error; translate it into a fix rather
// than surfacing the raw GDBus stack. Non-Linux failures pass through unchanged.

/** Does this stderr indicate that no notification server exists on the bus? */
export function isNoNotificationServer(stderr: string): boolean {
  const s = stderr.toLowerCase();
  return s.includes("org.freedesktop.notifications") && (
    s.includes("serviceunknown") ||
    s.includes("not provided by any") ||
    s.includes("was not provided") ||
    s.includes("not activatable")
  );
}

/** Actionable message for a failed notification command (exit `code`). */
export function notifyFailureMessage(code: number, stderr: string): string {
  if (isNoNotificationServer(stderr)) {
    return (
      "no desktop notification daemon is running, so the notification was not " +
      "shown. Bare window managers (i3/dwm/bspwm/sway) don't start one by " +
      "default — run dunst (X11) or mako (Wayland). See " +
      "tasks/buildin/notify/README.md. Original error: " + stderr
    );
  }
  return `notification command failed (exit ${code}): ${stderr}`;
}
