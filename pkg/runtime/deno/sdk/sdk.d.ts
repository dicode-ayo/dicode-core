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

declare interface DicodeCrypto {
  /** Encrypt arbitrary bytes under a context string. The context is bound
   *  into AEAD AAD; blobs from one context cannot be decrypted under another.
   *  Requires permissions.dicode.crypto: ["context", ...]. */
  encrypt: (context: string, plaintext: Uint8Array) => Promise<Uint8Array>;
  /** Decrypt a blob produced by encrypt() under the same context. */
  decrypt: (context: string, ciphertext: Uint8Array) => Promise<Uint8Array>;
}

declare interface DicodeSecrets {
  /** Returns true if a secret with the given key exists in the store.
   *  Never returns the value. Requires permissions.dicode.secrets_has: true. */
  has: (key: string) => Promise<boolean>;
}

/** Summary returned for one task by `dicode.list_tasks()`. The IPC server
 *  trims the spec to fields already visible in /api/tasks; filesystem paths
 *  and permission details are deliberately not exposed. */
declare interface TaskSummary {
  id:           string;
  name:         string;
  description?: string;
  params?:      unknown;
  /** Optional namespaced template marker (e.g. "dicode.io/oauth-app").
   *  Set in the base task.yaml; propagates to every entry that inherits
   *  via `ref.path`. Use this to discover instances of a template at runtime
   *  without hardcoding a per-task allowlist. */
  template?:    string;
  /** Webhook path for tasks with a webhook trigger (e.g. "/hooks/google-oauth"). */
  webhook?:     string;
  enabled:      boolean;
}

/** A JSON Schema (draft 2020-12) object describing the input a suspended task
 *  collects before resume (#512). The daemon validates the submission against
 *  it server-side. Loosely typed — any valid JSON Schema is accepted; the
 *  default WebUI renderer understands `type`, `properties`, `required`, `enum`,
 *  `default`, `title`, `description`, and `format`. */
declare type JSONSchema = {
  type?: string;
  title?: string;
  description?: string;
  properties?: Record<string, JSONSchema>;
  required?: string[];
  enum?: unknown[];
  default?: unknown;
  format?: string;
  [keyword: string]: unknown;
};

/** Argument to dicode.suspend() (#512). `state` is an opaque JSON blob echoed
 *  back as ctx.resume_state on resume; `schema` is the JSON Schema the
 *  submitted input is validated against; `deadline` is an optional Unix-ms
 *  TTL.
 *
 *  `state` is REQUIRED — it is the only signal separating a first run
 *  (ctx.resume_state undefined) from a resume (ctx.resume_state = this object).
 *  A missing/null state reads back as undefined on resume, re-firing an
 *  `if (!resume_state)` guard and re-suspending forever. Pass at least `{}`. */
declare interface SuspendRequest {
  state: unknown;
  schema: JSONSchema;
  deadline?: number;
}

declare interface Dicode {
  /** Fully-namespaced id of the currently-running task (e.g. "buildin/ai-agent"). */
  task_id:        string;
  /** Id of the current run (uuid). */
  run_id:         string;
  run_task:       (taskID: string, params?: Record<string, string>) => Promise<unknown>;
  list_tasks:     ()                                                 => Promise<TaskSummary[]>;
  get_runs:       (taskID: string, opts?: { limit?: number })        => Promise<unknown>;
  /** Pause the run: hand the runtime `state` + `schema`, then never resolve —
   *  the process exits cleanly and the run ends as `suspended`. On resume the
   *  task re-runs with ctx.resume_state / ctx.resume_input populated (#512). */
  suspend:        (req: SuspendRequest) => Promise<never>;
  secrets_set:    (key: string, value: string) => Promise<void>;
  secrets_delete: (key: string)                => Promise<void>;
  /** Secrets presence API. Requires permissions.dicode.secrets_has: true. */
  secrets:        DicodeSecrets;
  crypto:         DicodeCrypto;
}

/** All SDK globals passed to your task's main() function. */
declare interface DicodeSdk {
  params: Params;
  kv:     KV;
  input:  unknown;
  /** Prior state blob when this run is a resume of a suspended one; the same
   *  value passed to dicode.suspend({ state }). Undefined on a first run (#95). */
  resume_state?: unknown;
  /** The user's form submission that triggered this resume, keyed by field
   *  name. Undefined on a first run (#95). */
  resume_input?: Record<string, unknown>;
  output: Output;
  mcp:    MCP;
  dicode: Dicode;
}
