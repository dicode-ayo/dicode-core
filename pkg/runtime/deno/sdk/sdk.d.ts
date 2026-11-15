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
  json:  (value: unknown)                               => Promise<void>;
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

/** One row of the security audit log. Params are sanitized at write time. */
declare interface AuditEvent {
  id:          string;
  ts:          string;
  event_type:  string;
  actor_kind:  string;
  actor_id:    string;
  target_kind: string;
  target_id:   string;
  params?:     string;
  run_id?:     string;
  allowed:     boolean;
  reason?:     string;
}

declare interface AuditQueryResult {
  events:      AuditEvent[];
  next_cursor: string;
}

declare interface DicodeAudit {
  /** Read the security audit trail. Pass `after` (a prior next_cursor) with
   *  order:"asc" to walk the log forward exactly-once.
   *  Requires permissions.dicode.audit_query. */
  query: (opts?: {
    after?:      string;
    limit?:      number;
    order?:      "asc" | "desc";
    taskID?:     string;
    actor?:      string;
    eventType?:  string;
  }) => Promise<AuditQueryResult>;
}

/** One entry of `dicode.sources.list()`. Host paths are withheld. */
declare interface SourceSummary {
  name:     string;
  type?:    string;
  url?:     string;
  branch?:  string;
  dev_mode: boolean;
}

/** Result of `dicode.sources.set_dev_mode()`. `clone_path` is the clone
 *  directory and `dev_root_path` its root taskset.yaml; both are absent when
 *  dev mode was disabled. */
declare interface SetDevModeResult {
  ok: boolean;
  dev_root_path?: string;
  clone_path?: string;
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

/** Argument to dicode.suspend() (#512). `schema` is the JSON Schema the
 *  submitted input is validated against; `deadline` is an optional Unix-ms TTL.
 *
 *  `to` names the step handler to run on resume (wizard shape: the runner
 *  dispatches `steps[to]`); omit it for the two-function (main + resume) or
 *  single-main shape. `state` is the author's carried blob, echoed back as
 *  ctx.state (unwrapped) on resume; the runner persists it wrapped with an
 *  internal step marker the author never sees. */
declare interface SuspendRequest {
  schema: JSONSchema;
  to?: string;
  state?: unknown;
  deadline?: number;
}

declare interface Dicode {
  /** Fully-namespaced id of the currently-running task (e.g. "buildin/ai-agent"). */
  task_id:        string;
  /** Id of the current run (uuid). */
  run_id:         string;
  /** Capabilities granted to this run. A dicode.* member is only callable
   *  when its capability is present; otherwise it fails with "permission denied". */
  caps:           string[];
  run_task:       (taskID: string, params?: Record<string, string>, opts?: { mcpContext?: boolean }) => Promise<unknown>;
  list_tasks:     (opts?: { mcpContext?: boolean })                  => Promise<TaskSummary[]>;
  get_runs:       (taskID: string, opts?: { limit?: number })        => Promise<unknown>;
  /** set_group labels the current run with a free-text string used by the
   *  WebUI to collapse same-group siblings (#116). Last write wins; only
   *  affects the current run. */
  set_group:      (label: string)      => Promise<void>;
  /** Pause the run: hand the runtime `schema` + carried `state`, then never
   *  resolve — the process exits cleanly and the run ends as `suspended`. On
   *  resume the runner dispatches the matching handler (steps[to], resume, or
   *  main) with ctx.state / ctx.input populated (#512). */
  suspend:        (req: SuspendRequest) => Promise<never>;
  secrets_set:    (key: string, value: string) => Promise<void>;
  secrets_delete: (key: string)                => Promise<void>;
  /** Secrets presence API. Requires permissions.dicode.secrets_has: true. */
  secrets:        DicodeSecrets;
  crypto:         DicodeCrypto;
  /** Security audit trail. Requires permissions.dicode.audit_query. */
  audit:          DicodeAudit;
  runs: {
    list_expired:  (opts?: { before_ts?: number }) => Promise<unknown>;
    delete_input:  (runID: string)                 => Promise<unknown>;
    /** Batched delete_input. */
    delete_inputs: (runIDs: string[])              => Promise<unknown>;
    pin_input:     (runID: string)                 => Promise<unknown>;
    unpin_input:   (runID: string)                 => Promise<unknown>;
    get_input:     (runID: string)                 => Promise<unknown>;
    replay:        (runID: string, taskName?: string) => Promise<unknown>;
    /** Like list_expired / delete_input, for offloaded dicode.suspend() state. */
    list_expired_resume_state: (opts?: { before_ts?: number }) => Promise<unknown>;
    delete_resume_state:       (runID: string)                 => Promise<unknown>;
  };
  tasks: {
    test: (taskID: string) => Promise<unknown>;
  };
  sources: {
    list: () => Promise<SourceSummary[]>;
    set_dev_mode: (name: string, opts: {
      enabled: boolean;
      local_path?: string;
      branch?: string;
      base?: string;
      run_id?: string;
    }) => Promise<SetDevModeResult>;
  };
  git: {
    commit_push: (sourceID: string, opts: {
      message: string;
      branch: string;
      branch_prefix?: string;
      allow_main?: boolean;
      files?: string[];
      author_name: string;
      author_email: string;
      auth_token_env?: string;
    }) => Promise<{ commit: string }>;
  };
}

/** The context each task handler (main / resume / steps[x]) receives (#512). */
declare interface DicodeSdk {
  params: Params;
  kv:     KV;
  /** The input handed to THIS invocation: the trigger payload on a fresh run,
   *  the schema-validated form submission on a resume. */
  input:  unknown;
  /** The author's carried blob, unwrapped from dicode.suspend({ state }).
   *  Undefined on a first run — tells first-vs-resume without a step switch. */
  state?: unknown;
  output: Output;
  mcp:    MCP;
  dicode: Dicode;
}

/** A task handler — the default export (`main`, the entry step), an optional
 *  `resume`, or a named entry in the `steps` map (#512). */
declare type TaskHandler = (ctx: DicodeSdk) => unknown | Promise<unknown>;

/** The wizard shape: a map of named step handlers. dicode.suspend({ to }) names
 *  the entry the runner dispatches on resume; `main` is the first step. */
declare type TaskSteps = Record<string, TaskHandler>;
