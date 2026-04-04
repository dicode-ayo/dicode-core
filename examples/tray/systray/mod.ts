// Vendored from https://deno.land/x/systray@v0.3.0
// Patches applied for Deno 2 compatibility:
//   - Deno.run → Deno.Command
//   - Deno.ProcessStatus → Deno.CommandStatus
//   - readLines (Reader-based) → streamLines (ReadableStream-based)
//   - Fixed linux arch mapping (x86_64/arm64 were swapped in original)
//   - Added onClick() convenience wrapper

import {
  base64Encode,
  configureCache,
  downloadAndCache,
  EventEmitter,
} from "./deps.ts";

const DEFAULT_VERSION = "v0.2.0";
const DEFAULT_URL_BASE = "https://github.com/wobsoriano/systray-portable/releases/download";

// Simple debug logger — only emits when conf.debug is true.
// deno.land/x/debug reads Deno.env.get("DEBUG") at import time which requires
// --allow-env; replacing it with a plain function avoids that requirement.
const log = (...args: unknown[]) => console.error("[systray]", ...args);

export interface MenuItem {
  title: string;
  tooltip: string;
  checked?: boolean;
  enabled?: boolean;
  hidden?: boolean;
  items?: MenuItem[];
  icon?: string;
  isTemplateIcon?: boolean;
}

interface MenuItemEx extends MenuItem {
  __id: number;
  items?: MenuItemEx[];
}

export interface Menu {
  icon: string;
  title: string;
  tooltip: string;
  items: MenuItem[];
  isTemplateIcon?: boolean;
}

export interface ClickEvent {
  type: "clicked";
  item: MenuItem;
  seq_id: number;
  __id: number;
}

export interface ReadyEvent {
  type: "ready";
}

export type Event = ClickEvent | ReadyEvent;

export interface UpdateItemAction {
  type: "update-item";
  item: MenuItem;
  seq_id?: number;
}

export interface UpdateMenuAction {
  type: "update-menu";
  menu: Menu;
}

export interface UpdateMenuAndItemAction {
  type: "update-menu-and-item";
  menu: Menu;
  item: MenuItem;
  seq_id?: number;
}

export interface ExitAction {
  type: "exit";
}

export type Action =
  | UpdateItemAction
  | UpdateMenuAction
  | UpdateMenuAndItemAction
  | ExitAction;

export interface Conf {
  menu: Menu;
  debug?: boolean;
  directory?: string | undefined;
  copyDir?: boolean;
  /** Systray portable binary version tag, e.g. "v0.2.0". Defaults to DEFAULT_VERSION. */
  version?: string;
}

// ── helpers ───────────────────────────────────────────────────────────────────

const CHECK_STR = " (√)";
function updateCheckedInLinux(item: MenuItem) {
  if (Deno.build.os !== "linux") return;
  if (item.checked) {
    item.title += CHECK_STR;
  } else {
    item.title = (item.title || "").replace(RegExp(CHECK_STR + "$"), "");
  }
  if (item.items != null) item.items.forEach(updateCheckedInLinux);
}

async function loadIcon(fileName: string) {
  const bytes = await Deno.readFile(fileName);
  return base64Encode(bytes.buffer as ArrayBuffer);
}

async function resolveIcon(item: MenuItem | Menu) {
  const icon = item.icon;
  if (icon) {
    try {
      item.icon = await loadIcon(icon);
    } catch {
      // Image not found
    }
  }
  if (item.items) await Promise.all(item.items.map((_) => resolveIcon(_)));
  return item;
}

function addInternalId(
  internalIdMap: Map<number, MenuItem>,
  item: MenuItemEx,
  counter = { id: 1 },
) {
  const id = counter.id++;
  internalIdMap.set(id, item);
  if (item.items != null) {
    item.items.forEach((_) => addInternalId(internalIdMap, _, counter));
  }
  item.__id = id;
}

function itemTrimmer(item: MenuItem) {
  return {
    title: item.title,
    tooltip: item.tooltip,
    checked: item.checked,
    enabled: item.enabled === undefined ? true : item.enabled,
    hidden: item.hidden,
    items: item.items,
    icon: item.icon,
    isTemplateIcon: item.isTemplateIcon,
    __id: (item as MenuItemEx).__id,
  };
}

function menuTrimmer(menu: Menu) {
  return {
    icon: menu.icon,
    title: menu.title,
    tooltip: menu.tooltip,
    items: menu.items.map(itemTrimmer),
    isTemplateIcon: menu.isTemplateIcon,
  };
}

function actionTrimmer(action: Action) {
  if (action.type === "update-item") {
    return { type: action.type, item: itemTrimmer(action.item), seq_id: action.seq_id };
  } else if (action.type === "update-menu") {
    return { type: action.type, menu: menuTrimmer(action.menu) };
  } else if (action.type === "update-menu-and-item") {
    return {
      type: action.type,
      item: itemTrimmer(action.item),
      menu: menuTrimmer(action.menu),
      seq_id: action.seq_id,
    };
  } else {
    return { type: action.type };
  }
}

// Reads newline-delimited text from a ReadableStream<Uint8Array>.
async function* streamLines(
  readable: ReadableStream<Uint8Array>,
): AsyncGenerator<string> {
  const reader = readable.getReader();
  const dec = new TextDecoder();
  let buf = "";
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      buf += dec.decode(value, { stream: true });
      const lines = buf.split("\n");
      buf = lines.pop()!;
      for (const line of lines) yield line;
    }
    if (buf) yield buf;
  } finally {
    reader.releaseLock();
  }
}

const getTrayPath = async (version: string) => {
  const base = `${DEFAULT_URL_BASE}/${version}`;
  const { arch, os } = Deno.build;
  let binName: string;

  switch (os) {
    case "windows":
      binName = arch === "x86_64"
        ? `${base}/tray_windows_amd64.exe`
        : `${base}/tray_windows_386.exe`;
      break;
    case "darwin":
      binName = arch === "x86_64"
        ? `${base}/tray_darwin_amd64`
        : `${base}/tray_darwin_arm64`;
      break;
    case "linux":
      // Note: original deno-systray@v0.3.0 had x86_64/arm64 swapped — fixed here.
      binName = arch === "x86_64"
        ? `${base}/tray_linux_amd64`
        : `${base}/tray_linux_arm64`;
      break;
    default:
      throw new Error(`Unsupported OS for tray: ${os}`);
  }

  const file = await downloadAndCache(binName);
  return file.path;
};

// ── SysTray class ─────────────────────────────────────────────────────────────

type Events = {
  data: [string];
  error: [string];
  exit: [Deno.CommandStatus];
  click: [ClickEvent];
  ready: [];
};

export default class SysTray extends EventEmitter<Events> {
  static separator: MenuItem = {
    title: "<SEPARATOR>",
    tooltip: "",
    enabled: true,
  };

  protected _conf: Conf;
  protected _binPath: string;
  private _process!: Deno.ChildProcess;
  private _stdin!: WritableStreamDefaultWriter<Uint8Array>;
  private _ready: Promise<void>;
  private internalIdMap = new Map<number, MenuItem>();

  constructor(conf: Conf) {
    super();
    this._conf = conf;
    this._binPath = null!;

    if (this._conf.directory) configureCache({ directory: this._conf.directory });

    this._ready = this.init();
  }

  private async run(binPath: string) {
    this._process = new Deno.Command(binPath, {
      stdin: "piped",
      stdout: "piped",
      stderr: "piped",
    }).spawn();

    this._stdin = this._process.stdin.getWriter();

    // Stream stdout lines → emit 'data'.
    (async () => {
      for await (const line of streamLines(this._process.stdout)) {
        if (line.trim()) this.emit("data", line);
      }
      const status = await this._process.status;
      this.emit("exit", status);
    })();

    // Stream stderr lines → emit 'error'.
    (async () => {
      for await (const line of streamLines(this._process.stderr)) {
        if (line.trim()) {
          if (this._conf.debug) log("onError", line, "binPath", this._binPath);
          this.emit("error", line);
        }
      }
    })();
  }

  private async init() {
    const conf = this._conf;
    this._binPath = await getTrayPath(this._conf.version ?? DEFAULT_VERSION);
    try {
      await Deno.chmod(this._binPath, 0o755);
    } catch {
      // chmod throws on Windows — safe to ignore
    }

    await this.run(this._binPath);

    conf.menu.items.forEach(updateCheckedInLinux);
    const counter = { id: 1 };
    conf.menu.items.forEach((_) =>
      addInternalId(this.internalIdMap, _ as MenuItemEx, counter)
    );
    await resolveIcon(conf.menu);

    this.once("ready", () => {
      this.writeLine(JSON.stringify(menuTrimmer(conf.menu)));
    });

    this.on("data", (line: string) => {
      const action: Event = JSON.parse(line);
      if (action.type === "clicked") {
        const item = this.internalIdMap.get(action.__id)!;
        action.item = Object.assign(item, action.item);
        if (this._conf.debug) log("%s, %o", "onClick", action);
        this.emit("click", action);
      } else if (action.type === "ready") {
        if (this._conf.debug) log("%s %o", "onReady", action);
        this.emit("ready");
      }
    });
  }

  ready() {
    return this._ready;
  }

  /** Convenience wrapper for on('click', handler). */
  onClick(handler: (event: ClickEvent) => void) {
    this.on("click", handler);
  }

  private writeLine(line: string) {
    if (!line) return;
    if (this._conf.debug) log("%s %o", "writeLine", line + "\n", "=====");
    const encoded = new TextEncoder().encode(`${line.trim()}\n`);
    this._stdin.write(encoded);
  }

  async sendAction(action: Action) {
    switch (action.type) {
      case "update-item":
        updateCheckedInLinux(action.item);
        if (action.seq_id == null) action.seq_id = -1;
        break;
      case "update-menu":
        action.menu = await resolveIcon(action.menu) as Menu;
        action.menu.items.forEach(updateCheckedInLinux);
        break;
      case "update-menu-and-item":
        action.menu = await resolveIcon(action.menu) as Menu;
        action.menu.items.forEach(updateCheckedInLinux);
        updateCheckedInLinux(action.item);
        if (action.seq_id == null) action.seq_id = -1;
        break;
    }
    if (this._conf.debug) log("%s %o", "sendAction", action);
    this.writeLine(JSON.stringify(actionTrimmer(action)));
    return this;
  }

  /**
   * Send exit action to the native helper and optionally exit the Deno process.
   * @param exitDeno Exit the Deno process after the helper exits. Default: true.
   */
  kill(exitDeno = true) {
    this.once("exit", () => {
      this._stdin.close().catch(() => {});
      if (exitDeno) Deno.exit();
    });
    this.sendAction({ type: "exit" });
  }

  get binPath() {
    return this._binPath;
  }

  get process() {
    return this._process;
  }
}
