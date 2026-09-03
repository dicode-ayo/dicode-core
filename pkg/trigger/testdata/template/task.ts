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

// Conditional-block directives let a template drop a whole section when a
// variable is unset or empty. Motivating case: relay.yaml lists ~15 OAuth
// providers whose client_ids are optional and usually unset; an unset value
// renders as YAML `null`, which the relay's schema rejects. Wrapping each
// provider block in a conditional makes the renderer omit the block entirely
// (so an OAuth-less relay renders valid config) instead of emitting a null.
//
//   #dicode:if VAR [VAR...]
//   ...lines...
//   #dicode:endif
//
// A block is kept only when at least one listed variable resolves to a
// non-empty string (OR semantics); otherwise the whole block — including any
// ${VAR} placeholders inside it — is dropped, so unset optional vars inside
// an omitted block never trip renderTemplate's unresolved-placeholder guard.
// Directive lines are matched after trimming surrounding whitespace, so they
// can be indented and read as YAML comments. Blocks may nest. Unbalanced
// if/endif fails loud.
const IF_RE = /^#\s*dicode:if\s+([A-Za-z_][A-Za-z0-9_ ]*?)\s*$/;
const ENDIF_RE = /^#\s*dicode:endif\s*$/;

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

// evalConditionals resolves #dicode:if / #dicode:endif blocks (see IF_RE),
// returning the template with excluded blocks and all directive lines
// removed. The result still contains ${VAR} placeholders in kept blocks —
// callers run renderTemplate on it afterwards. `get` resolves a condition
// variable to its value; a block is kept when any of its variables resolves
// to a non-empty string. A getter that throws (scoped --allow-env
// PermissionDenied) is treated as "unset" for that variable.
export function evalConditionals(
  template: string,
  get: (name: string) => string | undefined,
): string {
  const out: string[] = [];
  // Stack of per-open-block "emitting" flags; a line is emitted only when
  // every enclosing block is active (so a nested block inside an excluded
  // one stays excluded regardless of its own condition).
  const stack: boolean[] = [];
  const emitting = () => stack.every((v) => v);
  for (const line of template.split("\n")) {
    const trimmed = line.trim();
    const ifm = trimmed.match(IF_RE);
    if (ifm) {
      if (!emitting()) {
        // Parent excluded: don't evaluate the nested condition at all.
        stack.push(false);
        continue;
      }
      const names = ifm[1].split(/\s+/).filter((n) => n !== "");
      const active = names.some((name) => {
        try {
          const v = get(name);
          return v !== undefined && v !== "";
        } catch {
          return false;
        }
      });
      stack.push(active);
      continue;
    }
    if (ENDIF_RE.test(trimmed)) {
      if (stack.length === 0) {
        throw new Error("unbalanced #dicode:endif (no matching #dicode:if)");
      }
      stack.pop();
      continue;
    }
    if (emitting()) {
      out.push(line);
    }
  }
  if (stack.length > 0) {
    throw new Error("unbalanced #dicode:if (missing #dicode:endif)");
  }
  return out.join("\n");
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
  // Require an absolute path. task.yaml documents this contract; enforce
  // it loudly here rather than letting a relative path resolve against
  // Deno's cwd (which would silently diverge from ${TASK_DIR} and
  // surprise operators). Mirrors buildin/write-local's path validation.
  if (!path.startsWith("/")) {
    throw new Error(
      `invalid template_path: must be absolute (got ${JSON.stringify(path)})`,
    );
  }
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

  // Resolve #dicode:if blocks first: excluded blocks (and their
  // placeholders) are dropped before renderTemplate ever sees them, so an
  // unset optional var inside an omitted block doesn't fail the render.
  const reduced = evalConditionals(body, (name) => Deno.env.get(name));

  // Drive env lookups by the placeholders found in the template. Using
  // per-name Deno.env.get() is required: Deno.env.toObject() needs
  // unrestricted --allow-env permission, but task.yaml's
  // permissions.env is a finite allowlist that compiles down to
  // `--allow-env=NAME1,NAME2,...`. Per-name get() works under both
  // scoped and unrestricted permissions and respects the allowlist.
  const names = collectPlaceholders(reduced);
  const envMap = buildEnvMap(names, (name) => Deno.env.get(name));

  // renderTemplate throws on any unresolved placeholder. Let it
  // propagate — the runtime turns the exception into a failed run,
  // which is the desired loud-failure contract.
  return renderTemplate(reduced, envMap);
}
