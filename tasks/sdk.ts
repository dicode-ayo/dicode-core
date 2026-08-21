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

export interface DicodeCrypto {
  /** Encrypt arbitrary bytes under a context string. Requires permissions.dicode.crypto. */
  encrypt(context: string, plaintext: Uint8Array): Promise<Uint8Array>;
  /** Decrypt a blob produced by encrypt() under the same context. */
  decrypt(context: string, ciphertext: Uint8Array): Promise<Uint8Array>;
}

// AuditEvent is one row of the security audit log. Params are sanitized at
// write time, so no value here is a raw secret.
export interface AuditEvent {
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

export interface AuditQueryResult {
  events:      AuditEvent[];
  next_cursor: string;
}

export interface DicodeAudit {
  /**
   * Read the security audit trail. Pass `after` (a prior result's
   * next_cursor) with order:"asc" to walk forward exactly-once.
   * Requires permissions.dicode.audit_query.
   */
  query(opts?: {
    after?:     string;
    limit?:     number;
    order?:     "asc" | "desc";
    taskID?:    string;
    actor?:     string;
    eventType?: string;
  }): Promise<AuditQueryResult>;
}

// A JSON Schema (draft 2020-12) object describing the input a suspended task
// asks the user to fill in before resume (#512). The daemon validates the
// submission against it server-side, so ctx.input conforms on resume.
// Loosely typed on purpose — any valid JSON Schema is accepted; the common
// keywords the default WebUI renderer understands are `type`, `properties`,
// `required`, `enum`, `default`, `title`, `description`, and `format`.
export type JSONSchema = {
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

// Argument to dicode.suspend() (#512). `schema` is the JSON Schema the submitted
// input is validated against; `deadline` is an optional Unix-ms TTL.
//
// `to` names the step handler to run on resume — the wizard shape: the runner
// dispatches `steps[to]`. Omit `to` for the two-function shape (main + resume)
// or a single-main task that branches by hand. `state` is the author's carried
// blob, echoed back as ctx.state (unwrapped) on the resume; the runner persists
// it wrapped with an internal step marker the author never sees.
export interface SuspendRequest {
  schema: JSONSchema;
  to?: string;
  state?: unknown;
  deadline?: number;
}

export interface Dicode {
  // Fully-namespaced id of the currently-running task (e.g. "buildin/ai-agent").
  // Populated from the IPC handshake; lets task code self-identify without
  // guessing from directory names.
  task_id: string;
  // Id of the current run (uuid).
  run_id: string;
  // The capability list the daemon granted this run, from the IPC handshake.
  // A member below is only callable when its capability is present; calling
  // one that is absent rejects with "permission denied".
  caps: string[];
  run_task(taskID: string, params?: Record<string, string>): Promise<unknown>;
  list_tasks(): Promise<unknown[]>;
  get_runs(taskID: string, opts?: { limit?: number }): Promise<unknown[]>;
  // set_group labels the current run with a free-text string used by the
  // WebUI to collapse same-group siblings (#116). Last write wins; only
  // affects the current run.
  set_group(label: string): Promise<void>;
  // Pause the run: hand the runtime `schema` + carried `state`, then never
  // resolve — the process exits cleanly and the run ends as `suspended`. On
  // resume the runner dispatches the matching handler (steps[to], resume, or
  // main) with ctx.state / ctx.input populated (#512).
  suspend(req: SuspendRequest): Promise<never>;
  secrets_set(key: string, value: string): Promise<void>;
  secrets_delete(key: string): Promise<void>;
  crypto: DicodeCrypto;
  audit: DicodeAudit;
  runs: DicodeRuns;
  tasks: DicodeTasks;
  sources: DicodeSources;
  git: DicodeGit;
}

// Run-input retention and replay. Every member is gated by its own
// permissions.dicode capability.
export interface DicodeRuns {
  list_expired(opts?: { before_ts?: number }): Promise<unknown>;
  delete_input(runID: string): Promise<unknown>;
  pin_input(runID: string): Promise<unknown>;
  unpin_input(runID: string): Promise<unknown>;
  get_input(runID: string): Promise<unknown>;
  // Re-fire a persisted run. taskName retargets the replay at a different task;
  // omit it to replay against the run's own task.
  replay(runID: string, taskName?: string): Promise<unknown>;
}

export interface DicodeTasks {
  // Run a task's sibling test file. Refused for a task the approval gate holds
  // pending, because the test file runs with full host permissions.
  test(taskID: string): Promise<unknown>;
}

// dev_root_path is the root taskset.yaml inside the clone and clone_path its
// directory — the value git-pr's clone_path expects. Both are absent when dev
// mode was disabled.
export interface SetDevModeResult {
  ok: boolean;
  dev_root_path?: string;
  clone_path?: string;
}

// One entry of dicode.sources.list(). Host paths are withheld from the
// listing; set_dev_mode is what returns a path, for the clone it just made.
export interface SourceSummary {
  name: string;
  type?: string;
  url?: string;
  branch?: string;
  dev_mode: boolean;
}

export interface DicodeSources {
  list(): Promise<SourceSummary[]>;
  set_dev_mode(name: string, opts: {
    enabled: boolean;
    // local_path and branch are mutually exclusive: local_path points dev-ref
    // resolution at an existing checkout, branch clones into a per-run dir.
    local_path?: string;
    branch?: string;
    base?: string;
    run_id?: string;
  }): Promise<SetDevModeResult>;
}

export interface DicodeGit {
  commit_push(sourceID: string, opts: {
    message: string;
    branch: string;
    branch_prefix?: string;
    allow_main?: boolean;
    files?: string[];
    author_name: string;
    author_email: string;
    // Must name an entry in the task's own permissions.env.
    auth_token_env?: string;
  }): Promise<{ commit: string }>;
}

// The context each task handler (main / resume / steps[x]) receives (#512).
export interface DicodeSdk {
  params: Params;
  kv:     KV;
  // The input handed to THIS invocation: the trigger payload on a fresh run, the
  // schema-validated form submission on a resume.
  input:  unknown;
  // The author's carried blob, unwrapped from dicode.suspend({ state }). Undefined
  // on a first run — so a handler can tell first-vs-resume without a step switch.
  state?: unknown;
  output: Output;
  mcp:    MCP;
  dicode: Dicode;
}

// A task handler — the default export (`main`, the entry step), an optional
// `resume`, or a named entry in the `steps` map (#512). Each receives the
// resume context and returns the run's result (or suspends again).
export type TaskHandler = (ctx: DicodeSdk) => unknown | Promise<unknown>;

// The wizard shape: a map of named step handlers. dicode.suspend({ to }) names
// the entry the runner dispatches on resume; `main` is the first step.
export type TaskSteps = Record<string, TaskHandler>;
