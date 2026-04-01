// dicode SDK shim — injected before every task script.
// Provides: log, params, env, kv, input, output, __setReturn__.
//
// Protocol: newline-delimited JSON over a single persistent Unix socket.
//   Fire-and-forget (no id):  log, kv.set, kv.delete, output
//   Request/response (id required): params, input, kv.get, kv.list, return

const __enc__ = new TextEncoder();
const __dec__ = new TextDecoder();
const __conn__ = await Deno.connect({
  transport: "unix",
  path: Deno.env.get("DICODE_SOCKET"),
});

// --- write infrastructure ---
// Serialise writes so concurrent fire-and-forget calls don't interleave.
let __wq__ = Promise.resolve();
function __write__(obj) {
  const bytes = __enc__.encode(JSON.stringify(obj) + "\n");
  __wq__ = __wq__.then(() => __conn__.write(bytes));
}

// --- read infrastructure ---
// A single background loop reads all response lines and dispatches them
// to the right pending promise by id.
const __pending__ = new Map(); // id → resolve
let __nid__ = 0;

(async () => {
  let buf = "";
  const tmp = new Uint8Array(4096);
  while (true) {
    let n;
    try { n = await __conn__.read(tmp); } catch { break; }
    if (n === null) break;
    buf += __dec__.decode(tmp.slice(0, n), { stream: true });
    let nl;
    while ((nl = buf.indexOf("\n")) >= 0) {
      const line = buf.slice(0, nl).trim();
      buf = buf.slice(nl + 1);
      if (!line) continue;
      try {
        const msg = JSON.parse(line);
        const resolve = __pending__.get(msg.id);
        if (resolve) { __pending__.delete(msg.id); resolve(msg); }
      } catch { /* ignore malformed lines */ }
    }
  }
})();

// Send a request that expects a response; returns Promise<result>.
function __call__(req) {
  const id = String(++__nid__);
  __write__({ ...req, id });
  return new Promise((resolve, reject) =>
    __pending__.set(id, (msg) =>
      msg.error ? reject(new Error(msg.error)) : resolve(msg.result),
    ),
  );
}

// Send a fire-and-forget request (no id, no response).
// Returns a resolved Promise so callers can safely `await` it.
function __fire__(req) {
  __write__(req);
  return Promise.resolve();
}

// --- log ---
function __fmt__(msg, args) {
  if (!args.length) return String(msg);
  return [String(msg), ...args.map((a) =>
    typeof a === "object" ? JSON.stringify(a) : String(a),
  )].join(" ");
}

const log = {
  info:  (msg, ...args) => __fire__({ method: "log", level: "info",  message: __fmt__(msg, args) }),
  warn:  (msg, ...args) => __fire__({ method: "log", level: "warn",  message: __fmt__(msg, args) }),
  error: (msg, ...args) => __fire__({ method: "log", level: "error", message: __fmt__(msg, args) }),
  debug: (msg, ...args) => __fire__({ method: "log", level: "debug", message: __fmt__(msg, args) }),
};

// --- params (lazy-fetched, deduplicated, cached) ---
let __params_fetch__ = null;
function __getParams__() {
  if (!__params_fetch__) __params_fetch__ = __call__({ method: "params" });
  return __params_fetch__;
}
const params = {
  get: async (key) => ((await __getParams__()) ?? {})[key] ?? null,
  all: () => __getParams__().then((p) => p ?? {}),
};

// --- env ---
const env = {
  get: (key) => Deno.env.get(key) ?? null,
};

// --- kv ---
// get/list are request/response; set/delete are fire-and-forget.
const kv = {
  get:    (key)         => __call__({ method: "kv.get", key }),
  set:    (key, value)  => __fire__({ method: "kv.set", key, value }),
  delete: (key)         => __fire__({ method: "kv.delete", key }),
  list:   (prefix = "") => __call__({ method: "kv.list", prefix }),
};

// --- input (fetched once at startup) ---
const input = await __call__({ method: "input" });

// --- output ---
// All methods are fire-and-forget; the Go side stores the result.
// Because writes are queued, the output message is guaranteed to arrive
// before the subsequent "return" message.
const output = {
  html: (content, opts) =>
    __fire__({ method: "output", contentType: "text/html", content, data: opts?.data ?? null }),
  text: (content) =>
    __fire__({ method: "output", contentType: "text/plain", content }),
  image: (mime, content) =>
    __fire__({ method: "output", contentType: mime ?? "image/png", content }),
  file: (name, content, mime) =>
    __fire__({ method: "output", contentType: mime ?? "application/octet-stream", content, data: { filename: name } }),
};

// --- return ---
async function __setReturn__(val) {
  await __call__({ method: "return", value: val ?? null });
}

// --- mcp ---
const mcp = {
  list_tools: (name)             => __call__({ method: "mcp.list_tools", mcpName: name }),
  call:       (name, tool, args) => __call__({ method: "mcp.call", mcpName: name, tool, args: args ?? {} }),
};
