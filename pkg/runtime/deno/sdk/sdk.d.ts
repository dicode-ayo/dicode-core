// dicode SDK ambient declarations — injected into Monaco via addExtraLib.
// No exports: ambient (non-module) .d.ts so all types are globally available
// without an import statement in task files.
//
// Logging: use console.log/warn/error/debug — no separate log global.

declare interface Params {
  get: (key: string) => Promise<string | null>;
  all: () => Promise<Record<string, string>>;
}

declare interface KV {
  get:    (key: string)                 => Promise<unknown>;
  set:    (key: string, value: unknown) => Promise<void>;
  delete: (key: string)                 => Promise<void>;
  list:   (prefix?: string)             => Promise<Record<string, unknown>>;
}

declare interface OutputOptions {
  data?: Record<string, unknown> | null;
}

declare interface Output {
  html:  (content: string, opts?: OutputOptions)        => Promise<void>;
  text:  (content: string)                              => Promise<void>;
  image: (mime: string | null, content: string)         => Promise<void>;
  file:  (name: string, content: string, mime?: string) => Promise<void>;
}

declare interface MCP {
  list_tools: (name: string)                                              => Promise<unknown>;
  call:       (name: string, tool: string, args?: Record<string, unknown>) => Promise<unknown>;
}

declare interface Dicode {
  run_task:       (taskID: string, params?: Record<string, string>) => Promise<unknown>;
  list_tasks:     ()                                                 => Promise<unknown>;
  get_runs:       (taskID: string, opts?: { limit?: number })        => Promise<unknown>;
  get_config:     (section: string)                                  => Promise<unknown>;
  secrets_set:    (key: string, value: string)                       => Promise<void>;
  secrets_delete: (key: string)                                       => Promise<void>;
}

/** All SDK globals passed to your task's main() function. */
declare interface DicodeSdk {
  params: Params;
  kv:     KV;
  input:  unknown;
  output: Output;
  mcp:    MCP;
  dicode: Dicode;
}
