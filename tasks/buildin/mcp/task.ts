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
//   list_sources     → hint pointing at GET /api/sources
//   switch_dev_mode  → hint pointing at PATCH /api/sources/{name}/dev
//   test_task        → hint pointing at POST /api/tasks/{id}/test
//
// The three "hint" tools used to be implemented in Go against private
// internals. As a task we don't have direct access to the SourceManager
// or tasktest — but every MCP client already has the dicode API base URL
// and an API key (that's how it reaches /mcp), so handing back a
// structured hint is enough for the client to complete the operation
// itself in one extra call.

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
      "Return a hint for listing sources. The MCP client should call GET /api/sources directly with its API key.",
    inputSchema: schema({}),
  },
  {
    name: "switch_dev_mode",
    description:
      "Return a hint for toggling dev mode on a taskset source. The MCP client should call PATCH /api/sources/{name}/dev directly.",
    inputSchema: schema(
      {
        source: { type: "string", description: "Source name" },
        enabled: { type: "boolean", description: "true to enable" },
        local_path: {
          type: "string",
          description: "Absolute path to a local taskset.yaml (when enabling local-path mode)",
        },
        branch: {
          type: "string",
          description:
            "Branch name to clone-and-checkout (when enabling clone-mode). Mutually exclusive with local_path.",
        },
        base: {
          type: "string",
          description:
            "Branch to fork from when `branch` does not exist remotely. Defaults to source's tracked branch.",
        },
        run_id: {
          type: "string",
          description:
            "Identifier for the per-fix clone directory; required with branch.",
        },
      },
      ["source", "enabled"],
    ),
  },
  {
    name: "test_task",
    description:
      "Return a hint for running a task's sibling test file. The MCP client should call POST /api/tasks/{id}/test directly.",
    inputSchema: schema(
      { id: { type: "string", description: "Namespaced task ID" } },
      ["id"],
    ),
  },
];

async function listTasks(dicode: Dicode): Promise<TaskSummary[]> {
  return ((await dicode.list_tasks()) as TaskSummary[]) ?? [];
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
      const result = await dicode.run_task(id, params);
      return textContent(JSON.stringify(result ?? null, null, 2));
    }
    case "list_sources": {
      return textContent(
        "Sources are not exposed via the dicode task SDK. Call `GET /api/sources` directly with your API key.",
      );
    }
    case "switch_dev_mode": {
      const src = String(args.source ?? "");
      if (!src) throw new Error("source is required");
      const body: Record<string, unknown> = { enabled: Boolean(args.enabled) };
      for (const k of ["local_path", "branch", "base", "run_id"]) {
        const v = args[k];
        if (typeof v === "string" && v) body[k] = v;
      }
      return textContent(
        `Dev-mode switching is not exposed via the dicode task SDK. ` +
          `Call \`PATCH /api/sources/${src}/dev\` with body ` +
          `${JSON.stringify(body)} directly.`,
      );
    }
    case "test_task": {
      const id = String(args.id ?? "");
      if (!id) throw new Error("id is required");
      return textContent(
        `Task tests are not exposed via the dicode task SDK. ` +
          `Call \`POST /api/tasks/${id}/test\` directly with your API key.`,
      );
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
