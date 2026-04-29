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

/** Secret output flag — daemon treats value as a flat Record<string,string>,
 *  routes it to the resolver awaiting this task, and redacts the values from
 *  the run log. Issue #119. */
declare interface SecretOutputOptions {
  secret: true;
}

declare interface Output {
  html:  (content: string, opts?: OutputOptions)        => Promise<void>;
  text:  (content: string)                              => Promise<void>;
  image: (mime: string | null, content: string)         => Promise<void>;
  file:  (name: string, content: string, mime?: string) => Promise<void>;
  /** Provider-task entry point (issue #119). Throws synchronously if `value`
   *  is not a flat Record<string,string>. */
  (value: Record<string, string>, opts: SecretOutputOptions): Promise<void>;
}

declare interface MCP {
  list_tools: (name: string)                                              => Promise<unknown>;
  call:       (name: string, tool: string, args?: Record<string, unknown>) => Promise<unknown>;
}

declare interface OAuthAuthURL {
  url:        string;
  session_id: string;
  provider:   string;
  timestamp:  number;
  relay_uuid: string;
}

declare interface OAuthStoreResult {
  provider: string;
  secrets:  string[];
}

declare interface ProviderStatus {
  provider:    string;
  has_token:   boolean;
  expires_at?: string;
  scope?:      string;
  token_type?: string;
}

declare interface DicodeOAuth {
  /** Requires permissions.dicode.oauth_init. Signs the daemon's side of a /auth/:provider URL via the relay broker. */
  build_auth_url: (provider: string, scope?: string) => Promise<OAuthAuthURL>;
  /** Requires permissions.dicode.oauth_store. Decrypts an incoming token envelope and writes the resulting credentials to secrets. Plaintext never crosses the IPC boundary. */
  store_token:    (envelope: unknown)                => Promise<OAuthStoreResult>;
  /** Requires permissions.dicode.oauth_status. Returns connection-state metadata (presence, expiry, scope) for the provider names supplied. Plaintext access/refresh tokens are never returned. */
  list_status:    (providers: string[])              => Promise<ProviderStatus[]>;
}

declare interface Dicode {
  /** Fully-namespaced id of the currently-running task (e.g. "buildin/ai-agent"). */
  task_id:        string;
  /** Id of the current run (uuid). */
  run_id:         string;
  run_task:       (taskID: string, params?: Record<string, string>) => Promise<unknown>;
  list_tasks:     ()                                                 => Promise<unknown>;
  get_runs:       (taskID: string, opts?: { limit?: number })        => Promise<unknown>;
  secrets_set:    (key: string, value: string)                       => Promise<void>;
  secrets_delete: (key: string)                                       => Promise<void>;
  oauth:          DicodeOAuth;
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
