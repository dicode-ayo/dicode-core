// Dicode SDK type definitions for task scripts.
// Import in task.ts: import type { DicodeSdk } from "../../sdk.ts";

export interface Params {
  get(key: string): Promise<string | null>;
  all(): Promise<Record<string, string>>;
}

export interface KV {
  get(key: string): Promise<unknown>;
  set(key: string, value: unknown): Promise<void>;
  delete(key: string): Promise<void>;
  list(prefix?: string): Promise<Record<string, unknown>>;
}

export interface Output {
  html(content: string, opts?: { data?: unknown }): Promise<void>;
  text(content: string): Promise<void>;
  image(mime: string, content: string): Promise<void>;
  file(name: string, content: string, mime?: string): Promise<void>;
}

export interface MCP {
  list_tools(name: string): Promise<unknown[]>;
  call(name: string, tool: string, args?: Record<string, unknown>): Promise<unknown>;
}

export interface Dicode {
  // Fully-namespaced id of the currently-running task (e.g. "buildin/ai-agent").
  // Populated from the IPC handshake; lets task code self-identify without
  // guessing from directory names.
  task_id: string;
  // Id of the current run (uuid).
  run_id: string;
  run_task(taskID: string, params?: Record<string, string>): Promise<unknown>;
  list_tasks(): Promise<unknown[]>;
  get_runs(taskID: string, opts?: { limit?: number }): Promise<unknown[]>;
  secrets_set(key: string, value: string): Promise<void>;
  secrets_delete(key: string): Promise<void>;
}

export interface DicodeSdk {
  params: Params;
  kv:     KV;
  input:  unknown;
  output: Output;
  mcp:    MCP;
  dicode: Dicode;
}
