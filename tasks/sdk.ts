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

// Provider-task entry point (issue #119): callable form for secret
// providers. The daemon treats `value` as a flat Record<string,string>,
// routes it to the resolver awaiting this task, and feeds the values to
// the run-log redactor. The non-callable methods preserve the legacy
// structured-output API.
export interface SecretOutputOptions {
  secret: true;
}

export interface Output {
  (value: Record<string, string>, opts: SecretOutputOptions): Promise<void>;
  html(content: string, opts?: { data?: unknown }): Promise<void>;
  text(content: string): Promise<void>;
  image(mime: string, content: string): Promise<void>;
  file(name: string, content: string, mime?: string): Promise<void>;
}

export interface MCP {
  list_tools(name: string): Promise<unknown[]>;
  call(name: string, tool: string, args?: Record<string, unknown>): Promise<unknown>;
}

export interface ProviderStatus {
  provider:    string;
  has_token:   boolean;
  expires_at?: string;
  scope?:      string;
  token_type?: string;
}

// OAuth broker bridge surface available on the dicode SDK. build_auth_url
// and store_token are reserved for buildin/auth-start and buildin/auth-relay;
// list_status is read-only metadata for any task that opts in via
// permissions.dicode.oauth_status.
export interface DicodeOAuth {
  build_auth_url(provider: string, scope?: string): Promise<unknown>;
  store_token(envelope: unknown): Promise<unknown>;
  list_status(providers: string[]): Promise<ProviderStatus[]>;
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
  oauth: DicodeOAuth;
}

export interface DicodeSdk {
  params: Params;
  kv:     KV;
  input:  unknown;
  output: Output;
  mcp:    MCP;
  dicode: Dicode;
}
