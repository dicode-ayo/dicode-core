import type { DicodeSdk } from "../sdk.ts";

export default async function main({ params, log }: DicodeSdk) {
// ── params ────────────────────────────────────────────────────────────────────

const title     = (await params.get("title"))    as string;
const body      = (await params.get("body"))     as string;
const priority  = ((await params.get("priority")) as string) ?? "default";
const tagsRaw   = ((await params.get("tags"))     as string) ?? "";
const iconParam = ((await params.get("icon"))     as string) ?? "";

if (!title) throw new Error("param 'title' is required");
if (!body)  throw new Error("param 'body' is required");

// ── urgency mapping ───────────────────────────────────────────────────────────
//   min | low → low      (notify-send urgency / WinForms ToolTipIcon::None)
//   default   → normal   (dialog-information / Info)
//   high | urgent → critical  (dialog-error / Warning)

type Urgency = "low" | "normal" | "critical";

function toUrgency(p: string): Urgency {
  switch (p) {
    case "min":
    case "low":    return "low";
    case "high":
    case "urgent": return "critical";
    default:       return "normal";
  }
}

// ── icon mapping ──────────────────────────────────────────────────────────────
// Linux:   freedesktop named icon (notify-send --icon).
//          If iconParam is set it overrides the auto-mapped name.
// Windows: ToolTipIcon enum value for NotifyIcon.ShowBalloonTip.
// macOS:   osascript display notification does not support custom icons.

function toLinuxIcon(urgency: Urgency): string {
  switch (urgency) {
    case "low":      return "dialog-information";
    case "critical": return "dialog-error";
    default:         return "dialog-information";
  }
}

function toWinIcon(urgency: Urgency): string {
  switch (urgency) {
    case "low":      return "None";
    case "critical": return "Warning";
    default:         return "Info";
  }
}

// ── build display body from tags ─────────────────────────────────────────────

const tags = tagsRaw.split(",").map((t: string) => t.trim()).filter(Boolean);
const displayBody = tags.length > 0 ? `[${tags.join(", ")}] ${body}` : body;

const urgency = toUrgency(priority);

// ── deliver notification via OS CLI ──────────────────────────────────────────
// No external Deno library — invoke the platform notification tool directly.

const os = Deno.build.os;
let cmd: string[];

if (os === "linux") {
  // notify-send requires the libnotify package (e.g. apt install libnotify-bin).
  // Check it exists before attempting to spawn so the error is actionable.
  const which = await new Deno.Command("which", { args: ["notify-send"] }).output();
  if (!which.success) {
    throw new Error(
      "notify-send not found — install libnotify (e.g. `apt install libnotify-bin` or `pacman -S libnotify`)"
    );
  }
  const icon = iconParam || toLinuxIcon(urgency);
  cmd = ["notify-send", "--urgency", urgency, "--icon", icon, title, displayBody];

} else if (os === "darwin") {
  // osascript is always present on macOS; display notification does not support
  // custom icons — the app icon of the Script Editor is used automatically.
  const escaped = (s: string) => s.replace(/\\/g, "\\\\").replace(/"/g, '\\"');
  cmd = [
    "osascript", "-e",
    `display notification "${escaped(displayBody)}" with title "${escaped(title)}"`,
  ];

} else if (os === "windows") {
  // PowerShell is always present on Windows. NotifyIcon.ShowBalloonTip delivers
  // a system tray balloon / toast (shown in the notification centre on Win 10+).
  // ToolTipIcon: None | Info | Warning | Error
  const winIcon = toWinIcon(urgency);
  const esc = (s: string) => s.replace(/'/g, "''"); // PowerShell single-quote escape
  const ps = [
    "Add-Type -AssemblyName System.Windows.Forms;",
    "$n = New-Object System.Windows.Forms.NotifyIcon;",
    "$n.Icon = [System.Drawing.SystemIcons]::Application;",
    "$n.Visible = $true;",
    `$n.ShowBalloonTip(5000, '${esc(title)}', '${esc(displayBody)}', [System.Windows.Forms.ToolTipIcon]::${winIcon});`,
    "Start-Sleep -Seconds 1;",
    "$n.Dispose()",
  ].join(" ");
  cmd = ["powershell", "-NoProfile", "-Command", ps];

} else {
  throw new Error(`Unsupported OS for notifications: ${os}`);
}

const result = await new Deno.Command(cmd[0], { args: cmd.slice(1) }).output();
if (!result.success) {
  const stderr = new TextDecoder().decode(result.stderr).trim();
  throw new Error(`notification command failed (exit ${result.code}): ${stderr}`);
}

await log.info("notification dispatched", { title, priority, urgency, tags });
return { title, urgency, tags };
}
