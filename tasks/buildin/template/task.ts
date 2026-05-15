// buildin/template — render ${VAR} placeholders in a template string
// using values from the task's environment.
//
// Invoke from another task:
//
//   const rendered = await dicode.run_task("buildin/template", {
//     template: "tunnel: ${ID}\nhost: ${HOST}",
//   });
//   // rendered === "tunnel: abc-123\nhost: api.example.com"
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
export function buildEnvMap(
  names: Iterable<string>,
  get: (name: string) => string | undefined,
): Record<string, string> {
  // Object.create(null): even if a placeholder spelled "__proto__"
  // slipped through, lookups through this map can't reach
  // Object.prototype.
  const map: Record<string, string> = Object.create(null);
  for (const name of names) {
    const v = get(name);
    if (v !== undefined) {
      map[name] = v;
    }
  }
  return map;
}

export default async function main({ params }: TaskSdk): Promise<string> {
  const template = (await params.get("template")) ?? "";

  // Drive env lookups by the placeholders found in the template. Using
  // per-name Deno.env.get() is required: Deno.env.toObject() needs
  // unrestricted --allow-env permission, but task.yaml's
  // permissions.env is a finite allowlist that compiles down to
  // `--allow-env=NAME1,NAME2,...`. Per-name get() works under both
  // scoped and unrestricted permissions and respects the allowlist.
  const names = collectPlaceholders(template);
  const envMap = buildEnvMap(names, (name) => Deno.env.get(name));

  // renderTemplate throws on any unresolved placeholder. Let it
  // propagate — the runtime turns the exception into a failed run,
  // which is the desired loud-failure contract.
  return renderTemplate(template, envMap);
}
