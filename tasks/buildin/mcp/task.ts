// MCP server, implemented as a dicode webhook task.
//
// Speaks JSON-RPC 2.0 over a single POST per call (the same shape the old
// pkg/mcp Go server spoke). The MCP client sends a request; the task
// reads it from `input`, dispatches the method, and returns the response
// as a bare JSON object — dicode serializes that with Content-Type
// application/json, which is exactly what MCP clients expect.
//
// Tool surface is intentionally a thin wrapper over the dicode SDK + the
// public HTTP API:
//   list_tasks       → dicode.list_tasks()
//   get_task         → list_tasks() filtered by id (one round-trip)
//   run_task         → dicode.run_task() (blocking; returns the run result)
//   list_sources     → dicode.sources.list()
//   switch_dev_mode  → dicode.sources.set_dev_mode()
//   test_task        → dicode.tasks.test()
//
// Who may call each tool is decided in pkg/webui's /mcp forwarder against
// the caller's token scope, before the request arrives here. This task
// holds every capability its tools need, so a check written here would
// only be a second opinion on a decision already made.

const PROTOCOL_VERSION = "2024-11-05";
const SERVER_NAME = "dicode";
const SERVER_VERSION = "dev";

interface JsonRpcRequest {
  jsonrpc?: string;
  id?: unknown;
  method?: string;
  params?: unknown;
}

interface JsonRpcError {
  code: number;
  message: string;
}

interface JsonRpcResponse {
  jsonrpc: "2.0";
  id: unknown;
  result?: unknown;
  error?: JsonRpcError;
}

interface ToolDef {
  name: string;
  description: string;
  inputSchema: Record<string, unknown>;
}

interface TaskSummary {
  id: string;
  name: string;
  description?: string;
  params?: unknown;
}

function ok(id: unknown, result: unknown): JsonRpcResponse {
  return { jsonrpc: "2.0", id, result };
}

function fail(id: unknown, code: number, message: string): JsonRpcResponse {
  return { jsonrpc: "2.0", id, error: { code, message } };
}

function textContent(text: string): Record<string, unknown> {
  return { content: [{ type: "text", text }] };
}

function schema(
  props: Record<string, unknown>,
  required: string[] = [],
): Record<string, unknown> {
  const s: Record<string, unknown> = { type: "object", properties: props };
  if (required.length > 0) s.required = required;
  return s;
}

const TOOLS: ToolDef[] = [
  {
    name: "list_tasks",
    description:
      "List all registered tasks with their IDs, names, descriptions, and declared params.",
    inputSchema: schema({}),
  },
  {
    name: "get_task",
    description:
      "Get the spec (id, name, description, params) for a single task by ID.",
    inputSchema: schema(
      { id: { type: "string", description: "Namespaced task ID" } },
      ["id"],
    ),
  },
  {
    name: "run_task",
    description:
      "Trigger a task by ID and wait for it to finish. Returns the task's run result.",
    inputSchema: schema(
      {
        id: { type: "string", description: "Namespaced task ID" },
        params: {
          type: "object",
          description: "Optional string-valued params to pass to the run",
          additionalProperties: { type: "string" },
        },
      },
      ["id"],
    ),
  },
  {
    name: "list_sources",
    description:
      "List the configured taskset sources: name, type, git URL, tracked " +
      "branch, and whether the source is currently in dev mode.",
    inputSchema: schema({}),
  },
  {
    name: "switch_dev_mode",
    description:
      "Enter or leave dev mode on a taskset source. Entering with a branch " +
      "clones the source repo into a scratch directory and returns " +
      "clone_path — edit files there, not in the live source. Leave dev mode " +
      "when done, which removes the clone locally and keeps the remote branch.",
    inputSchema: schema(
      {
        source: { type: "string", description: "Source name" },
        enabled: { type: "boolean", description: "true to enable" },
        branch: {
          type: "string",
          description: "Branch name to clone and check out (clone mode).",
        },
        base: {
          type: "string",
          description:
            "Branch to fork from when `branch` does not exist remotely. Defaults to source's tracked branch.",
        },
        run_id: {
          type: "string",
          description:
            "Names the per-session clone directory; required with branch. " +
            "Ignored for callers holding a per-run token — the daemon binds " +
            "it to the minting run.",
        },
      },
      ["source", "enabled"],
    ),
  },
  {
    name: "test_task",
    description:
      "Run a task's sibling test file (task.test.ts / task.test.py) and " +
      "return the results. Refused for a task the approval gate is still " +
      "holding pending, because the test file runs with full host permissions.",
    inputSchema: schema(
      { id: { type: "string", description: "Namespaced task ID" } },
      ["id"],
    ),
  },
];

async function listTasks(dicode: Dicode): Promise<TaskSummary[]> {
  // mcpContext: true signals the IPC server to filter to tasks with
  // mcp_exposed: true — so only explicitly opted-in tasks are discoverable
  // via the MCP endpoint.
  return ((await dicode.list_tasks({ mcpContext: true })) as TaskSummary[]) ?? [];
}

function argStr(args: Record<string, unknown>, key: string): string {
  const v = args[key];
  return typeof v === "string" ? v : "";
}

// Matches argBool in tasks/buildin/ai-agent/task.ts: the same argument reaches
// set_dev_mode from both surfaces and must mean the same thing on each. A bare
// Boolean() would read the string "false" as true and enter dev mode for a
// caller asking to leave it — the schema says boolean, but nothing between an
// MCP client and here enforces that.
function argBool(args: Record<string, unknown>, key: string): boolean {
  const v = args[key];
  return typeof v === "string" ? v === "true" : Boolean(v);
}

function stringifyParams(raw: unknown): Record<string, string> {
  const out: Record<string, string> = {};
  if (raw && typeof raw === "object") {
    for (const [k, v] of Object.entries(raw as Record<string, unknown>)) {
      out[k] = typeof v === "string" ? v : JSON.stringify(v);
    }
  }
  return out;
}

async function dispatchTool(
  name: string,
  args: Record<string, unknown>,
  dicode: Dicode,
): Promise<unknown> {
  switch (name) {
    case "list_tasks": {
      const all = await listTasks(dicode);
      return textContent(JSON.stringify(all, null, 2));
    }
    case "get_task": {
      const id = String(args.id ?? "");
      if (!id) throw new Error("id is required");
      const all = await listTasks(dicode);
      const found = all.find((t) => t.id === id);
      if (!found) throw new Error(`task ${JSON.stringify(id)} not found`);
      return textContent(JSON.stringify(found, null, 2));
    }
    case "run_task": {
      const id = String(args.id ?? "");
      if (!id) throw new Error("id is required");
      const params = stringifyParams(args.params);
      // mcpContext: true gates invocation to tasks with mcp_exposed: true.
      const result = await dicode.run_task(id, params, { mcpContext: true });
      return textContent(JSON.stringify(result ?? null, null, 2));
    }
    case "list_sources": {
      const sources = await dicode.sources.list();
      return textContent(JSON.stringify(sources, null, 2));
    }
    case "switch_dev_mode": {
      const src = String(args.source ?? "");
      if (!src) throw new Error("source is required");
      // local_path is not a tool argument: it redirects the daemon's taskset
      // resolution at an arbitrary host path, which would let a caller decide
      // what the daemon loads as tasks. Operators reach that mode through
      // PATCH /api/sources/{name}/dev.
      const result = await dicode.sources.set_dev_mode(src, {
        enabled: argBool(args, "enabled"),
        branch: argStr(args, "branch"),
        base: argStr(args, "base"),
        run_id: argStr(args, "run_id"),
      });
      return textContent(JSON.stringify(result, null, 2));
    }
    case "test_task": {
      const id = String(args.id ?? "");
      if (!id) throw new Error("id is required");
      const result = await dicode.tasks.test(id);
      return textContent(JSON.stringify(result ?? null, null, 2));
    }
    default:
      throw new Error(`unknown tool: ${name}`);
  }
}

async function handle(
  req: JsonRpcRequest,
  dicode: Dicode,
): Promise<JsonRpcResponse> {
  const id = req.id ?? null;
  const method = req.method ?? "";

  switch (method) {
    case "initialize":
      return ok(id, {
        protocolVersion: PROTOCOL_VERSION,
        capabilities: { tools: {} },
        serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
      });

    case "tools/list":
      return ok(id, { tools: TOOLS });

    case "tools/call": {
      const params = (req.params ?? {}) as {
        name?: string;
        arguments?: Record<string, unknown>;
      };
      if (!params.name) return fail(id, -32602, "tool name is required");
      try {
        const result = await dispatchTool(
          params.name,
          params.arguments ?? {},
          dicode,
        );
        return ok(id, result);
      } catch (e) {
        const msg = e instanceof Error ? e.message : String(e);
        return fail(id, -32603, msg);
      }
    }

    default:
      return fail(id, -32601, `method not found: ${method}`);
  }
}

export default async function main({ input, dicode }: DicodeSdk) {
  // GET requests don't reach here — the wrapping /mcp URL handler answers
  // those with a small server-info JSON. For POST requests, `input` is
  // the parsed JSON body; missing/invalid bodies surface as a JSON-RPC
  // parse error so the MCP client sees a well-formed envelope rather
  // than a dicode-shaped 500.
  if (!input || typeof input !== "object") {
    return fail(null, -32700, "parse error: empty or non-object body");
  }
  return handle(input as JsonRpcRequest, dicode);
}
