// Dicode SDK type definitions for task scripts.
// Import in task.ts: import type { DicodeSdk } from "../sdk.ts";

export interface Log {
  info(msg: unknown, ...args: unknown[]): Promise<void>;
  warn(msg: unknown, ...args: unknown[]): Promise<void>;
  error(msg: unknown, ...args: unknown[]): Promise<void>;
  debug(msg: unknown, ...args: unknown[]): Promise<void>;
}

export interface Params {
  get(key: string): Promise<string | null>;
  all(): Promise<Record<string, string>>;
}

export interface Env {
  get(key: string): string | null;
}

export interface KV {
  get(key: string): Promise<unknown>;
  set(key: string, value: unknown): Promise<void>;
  delete(key: string): Promise<void>;
  list(prefix?: string): Promise<Array<{ key: string; value: unknown }>>;
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
  run_task(taskID: string, params?: Record<string, string>): Promise<unknown>;
  list_tasks(): Promise<unknown[]>;
  get_runs(taskID: string, opts?: { limit?: number }): Promise<unknown[]>;
  get_config(section: string): Promise<unknown>;
}

export interface DicodeSdk {
  log: Log;
  params: Params;
  env: Env;
  kv: KV;
  input: unknown;
  output: Output;
  mcp: MCP;
  dicode: Dicode;
}
