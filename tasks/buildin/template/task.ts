// buildin/template — render ${VAR} placeholders in a template body
// using values from the task's environment.
//
// The template body is supplied either inline as a string (template
// param) or by absolute path to a sibling file (template_path param).
// Exactly one of the two is required; both or neither fails loudly.
//
// Invoke from another task:
//
//   const rendered = await dicode.run_task("buildin/template", {
//     template: "tunnel: ${ID}\nhost: ${HOST}",
//   });
//   // rendered === "tunnel: abc-123\nhost: api.example.com"
//
//   // ...or, for large templates, point at a sibling file:
//   const rendered2 = await dicode.run_task("buildin/template", {
//     template_path: "/abs/path/to/template.tpl",
//   });
//
// The rendered string is returned from main() — the runtime ships it
// back as the run result. task.yaml sets `run_result.enabled: false`,
// so the value flows in-memory to synchronous run_task callers and to
// chained downstreams via input.output but is NOT written to
// runs.return_value (where the WebUI / REST API would surface it).
//
// Unresolved placeholders fail loudly: silent passthrough is a footgun
// for config files that embed credentials. Prototype-chain lookups
// (${toString}, ${__proto__}, ...) are blocked by a null-prototype env
// map plus hasOwnProperty guards in renderTemplate.

// PLACEHOLDER_RE captures the placeholder name. The shape
// [A-Za-z_][A-Za-z0-9_]* mirrors POSIX env-var naming, which is also
// what Deno.env keys must satisfy on every supported platform.
const PLACEHOLDER_RE = /\$\{([A-Za-z_][A-Za-z0-9_]*)\}/g;

// Minimal local SDK shape. The full DicodeSdk type is an ambient
// declaration injected by the daemon at runtime; it isn't visible to
// `deno test` (which type-checks this file because the test imports
// renderTemplate from it). We narrow to just the surface we touch.
interface TaskSdk {
  params: { get(key: string): Promise<string | null> };
}

export function renderTemplate(
  template: string,
  env: Record<string, string>,
): string {
  return template.replace(PLACEHOLDER_RE, (_match, name) => {
    // hasOwnProperty.call — never bare `name in env` and never
    // `env[name]` without an ownership check. `${toString}`,
    // `${constructor}`, `${__proto__}`, etc. would otherwise resolve to
    // Object.prototype members and silently embed function source into
    // the rendered config — bypassing the loud-failure contract.
    if (!Object.prototype.hasOwnProperty.call(env, name)) {
      throw new Error(`unresolved placeholder: \${${name}}`);
    }
    return env[name];
  });
}

// collectPlaceholders returns the set of unique placeholder names in a
// template. Exported for testing; main() uses it to drive per-name
// Deno.env.get() lookups (which is the only path compatible with
// scoped --allow-env permissions; Deno.env.toObject() requires
// unrestricted --allow-env and would throw under our sandbox).
export function collectPlaceholders(template: string): string[] {
  const names = new Set<string>();
  for (const m of template.matchAll(PLACEHOLDER_RE)) {
    names.add(m[1]);
  }
  return [...names];
}

// buildEnvMap looks up each placeholder name via the supplied getter
// and returns a null-prototype map of resolved values. Missing keys are
// simply omitted; renderTemplate raises a loud error when it
// encounters them, so the failure surface is the placeholder name (not
// "undefined env"). The getter is injected for testability.
//
// A getter that throws (e.g. Deno.env.get under scoped --allow-env can
// throw PermissionDenied for names outside the allowlist on some
// platforms) is treated identically to "name not declared" — the entry
// is omitted, and renderTemplate later fails with the loud
// "unresolved placeholder: ${NAME}" message. That keeps the
// user-facing error consistent regardless of whether the runtime
// reports an undeclared name via `undefined` or via a permission
// throw.
export function buildEnvMap(
  names: Iterable<string>,
  get: (name: string) => string | undefined,
): Record<string, string> {
  // Object.create(null): even if a placeholder spelled "__proto__"
  // slipped through, lookups through this map can't reach
  // Object.prototype.
  const map: Record<string, string> = Object.create(null);
  for (const name of names) {
    let v: string | undefined;
    try {
      v = get(name);
    } catch {
      // Treat any throw (PermissionDenied, etc.) as "not declared".
      // renderTemplate will surface the loud unresolved-placeholder
      // error, which is the right user-facing diagnostic.
      v = undefined;
    }
    if (v !== undefined) {
      map[name] = v;
    }
  }
  return map;
}

// resolveTemplateBody enforces the XOR contract between the inline
// `template` param and the file-backed `template_path` param. Exported
// for testability — the file-IO path is small but worth a unit test.
//
// Both supplied → loud failure: exactly one is required.
// Neither supplied → loud failure: at least one is required.
// `template` supplied → use it verbatim.
// `template_path` supplied → read the file; wrap IO errors with a clear
// "reading template_path <path>: <reason>" message so the user sees the
// path that failed (ENOENT, EACCES, etc.) instead of a bare Deno error.
//
// Empty-string is treated as "not supplied" — a zero-length value for
// either param is almost certainly a misconfiguration (unresolved
// caller-side ${VAR}, or an empty default fallback) rather than an
// intentional empty template. The XOR check uses `!= null && != ""`
// so an empty value behaves the same as a missing param.
export async function resolveTemplateBody(
  template: string | null,
  templatePath: string | null,
  readFile: (path: string) => Promise<string> = Deno.readTextFile,
): Promise<string> {
  const hasTemplate = template !== null && template !== "";
  const hasPath = templatePath !== null && templatePath !== "";

  if (hasTemplate && hasPath) {
    throw new Error(
      "both template and template_path supplied; exactly one is required",
    );
  }
  if (!hasTemplate && !hasPath) {
    throw new Error(
      "either template or template_path must be supplied",
    );
  }
  if (hasTemplate) {
    return template as string;
  }
  // hasPath
  const path = templatePath as string;
  try {
    return await readFile(path);
  } catch (err) {
    const msg = err instanceof Error ? err.message : String(err);
    throw new Error(`reading template_path ${path}: ${msg}`);
  }
}

export default async function main({ params }: TaskSdk): Promise<string> {
  // XOR validation lives in resolveTemplateBody. task.yaml no longer
  // marks `template` as required: the trigger engine therefore can't
  // reject "neither supplied" upstream, so we own the loud-failure
  // contract entirely here.
  const template = await params.get("template");
  const templatePath = await params.get("template_path");
  const body = await resolveTemplateBody(template, templatePath);

  // Drive env lookups by the placeholders found in the template. Using
  // per-name Deno.env.get() is required: Deno.env.toObject() needs
  // unrestricted --allow-env permission, but task.yaml's
  // permissions.env is a finite allowlist that compiles down to
  // `--allow-env=NAME1,NAME2,...`. Per-name get() works under both
  // scoped and unrestricted permissions and respects the allowlist.
  const names = collectPlaceholders(body);
  const envMap = buildEnvMap(names, (name) => Deno.env.get(name));

  // renderTemplate throws on any unresolved placeholder. Let it
  // propagate — the runtime turns the exception into a failed run,
  // which is the desired loud-failure contract.
  return renderTemplate(body, envMap);
}
