// Unit tests for the buildin/template task.
//
// renderTemplate is tested directly — its per-character behavior
// matters more than the IPC round-trip. main() is tested with a stub
// SDK and Deno.env to verify the end-to-end contract: template comes
// in via params.get, env vars resolve via Deno.env.get, rendered
// string is returned.

import { assertEquals, assertRejects, assertThrows } from "jsr:@std/assert@1";
import {
  buildEnvMap,
  collectPlaceholders,
  renderTemplate,
  resolveTemplateBody,
} from "./task.ts";

// --- renderTemplate (pure) ---

Deno.test("renderTemplate: substitutes ${VAR} placeholders", () => {
  const out = renderTemplate(
    "tunnel: ${TUNNEL_ID}\nhost: ${HOST}",
    { TUNNEL_ID: "abc-123", HOST: "api.example.com" },
  );
  assertEquals(out, "tunnel: abc-123\nhost: api.example.com");
});

Deno.test("renderTemplate: rejects unresolved placeholder", () => {
  assertThrows(
    () => renderTemplate("hello ${MISSING}", {}),
    Error,
    "unresolved placeholder: ${MISSING}",
  );
});

Deno.test("renderTemplate: strict placeholder regex leaves non-matches alone", () => {
  // Only [A-Za-z_][A-Za-z0-9_]* matches as a placeholder. Bash-style
  // expansions like ${1} or ${a-b} and bare $VAR are not touched.
  const out = renderTemplate(
    "$1 ${a-b} $VALID ${VALID}",
    { VALID: "x" },
  );
  assertEquals(out, "$1 ${a-b} $VALID x");
});

Deno.test("renderTemplate: empty value substitutes empty string", () => {
  const out = renderTemplate("a=${EMPTY}b", { EMPTY: "" });
  assertEquals(out, "a=b");
});

Deno.test("renderTemplate: same placeholder occurs multiple times", () => {
  const out = renderTemplate("${X}-${X}-${X}", { X: "y" });
  assertEquals(out, "y-y-y");
});

Deno.test("renderTemplate: prototype keys are not resolvable", () => {
  // Regression: `name in env` would walk the prototype chain and
  // resolve ${toString}, ${constructor}, ${__proto__}, etc. to
  // Object.prototype members — bypassing the loud-failure contract.
  // The implementation uses Object.prototype.hasOwnProperty.call to
  // keep lookups strictly own-property.
  for (
    const evil of [
      "toString",
      "constructor",
      "__proto__",
      "hasOwnProperty",
      "valueOf",
    ]
  ) {
    assertThrows(
      () => renderTemplate(`x=\${${evil}}`, {}),
      Error,
      `unresolved placeholder: \${${evil}}`,
    );
  }
});

Deno.test("renderTemplate: env on a null-prototype object still works", () => {
  // The runtime path builds a null-prototype env map. Ensure the
  // hasOwnProperty guard works for that case (it must not blow up on
  // an object whose own [[Prototype]] is null).
  const env: Record<string, string> = Object.create(null);
  env.A = "1";
  env.B = "2";
  assertEquals(renderTemplate("${A}/${B}", env), "1/2");
});

// --- collectPlaceholders (pure) ---

Deno.test("collectPlaceholders: returns unique names in encounter order", () => {
  const names = collectPlaceholders("${A} ${B} ${A} ${C}");
  assertEquals(names, ["A", "B", "C"]);
});

Deno.test("collectPlaceholders: empty when template has none", () => {
  assertEquals(collectPlaceholders("no placeholders here"), []);
});

Deno.test("collectPlaceholders: ignores invalid placeholder shapes", () => {
  // Bash-style ${1}, ${a-b}, bare $VAR don't match the strict regex.
  assertEquals(collectPlaceholders("$1 ${a-b} $X ${OK}"), ["OK"]);
});

// --- buildEnvMap (pure) ---

Deno.test("buildEnvMap: looks up each name via the getter", () => {
  const env: Record<string, string> = { A: "1", B: "2" };
  const map = buildEnvMap(["A", "B"], (n) => env[n]);
  assertEquals(map.A, "1");
  assertEquals(map.B, "2");
});

Deno.test("buildEnvMap: omits names the getter doesn't know", () => {
  const map = buildEnvMap(["A", "MISSING"], (n) => n === "A" ? "x" : undefined);
  assertEquals(Object.prototype.hasOwnProperty.call(map, "A"), true);
  assertEquals(Object.prototype.hasOwnProperty.call(map, "MISSING"), false);
});

Deno.test("buildEnvMap: returned map has null prototype", () => {
  const map = buildEnvMap(["A"], () => "x");
  assertEquals(Object.getPrototypeOf(map), null);
});

Deno.test("buildEnvMap: getter that throws is treated as not-declared", () => {
  // Under scoped --allow-env, some Deno builds throw PermissionDenied
  // from env.get(name) for names that aren't in the allowlist (instead
  // of returning undefined). buildEnvMap must catch that so the
  // downstream renderTemplate failure mode is the user-facing
  // "unresolved placeholder: ${NAME}" — not a permission-denied stack.
  const map = buildEnvMap(["DECLARED", "UNDECLARED"], (n) => {
    if (n === "UNDECLARED") {
      throw new Deno.errors.PermissionDenied(
        `Requires env access to "${n}"`,
      );
    }
    return "ok";
  });
  assertEquals(Object.prototype.hasOwnProperty.call(map, "DECLARED"), true);
  assertEquals(map.DECLARED, "ok");
  assertEquals(Object.prototype.hasOwnProperty.call(map, "UNDECLARED"), false);
});

// --- main() end-to-end ---

interface ParamStub {
  get(key: string): Promise<string | null>;
}
interface SdkStub {
  params: ParamStub;
}

function stubSdk(paramVals: Record<string, string>): SdkStub {
  return {
    params: {
      get(key: string): Promise<string | null> {
        return Promise.resolve(paramVals[key] ?? null);
      },
    },
  };
}

// Re-import main with the right shape. We avoid a direct `default`
// import re-export to keep the test file's typecheck honest.
const taskMod = await import("./task.ts");
const main = (taskMod as unknown as {
  default: (sdk: SdkStub) => Promise<string>;
}).default;

// Run each main() test under a temporary Deno.env scope so the
// per-test env overrides don't leak.
async function withEnv(
  vars: Record<string, string>,
  fn: () => Promise<void>,
): Promise<void> {
  const prior: Record<string, string | undefined> = {};
  for (const k of Object.keys(vars)) {
    prior[k] = Deno.env.get(k);
    Deno.env.set(k, vars[k]);
  }
  try {
    await fn();
  } finally {
    for (const [k, v] of Object.entries(prior)) {
      if (v === undefined) Deno.env.delete(k);
      else Deno.env.set(k, v);
    }
  }
}

Deno.test("main: reads template param, resolves env, returns rendered string", async () => {
  await withEnv(
    { TPL_TEST_ID: "abc-123", TPL_TEST_HOST: "api.example.com" },
    async () => {
      const out = await main(stubSdk({
        template: "tunnel: ${TPL_TEST_ID}\nhost: ${TPL_TEST_HOST}",
      }));
      assertEquals(out, "tunnel: abc-123\nhost: api.example.com");
    },
  );
});

Deno.test("main: missing env var produces unresolved-placeholder error", async () => {
  // Ensure the var is not present.
  Deno.env.delete("TPL_TEST_DEFINITELY_UNSET_VAR");
  await assertRejects(
    () =>
      main(stubSdk({
        template: "x=${TPL_TEST_DEFINITELY_UNSET_VAR}",
      })),
    Error,
    "unresolved placeholder: ${TPL_TEST_DEFINITELY_UNSET_VAR}",
  );
});

Deno.test("main: empty template treated as not supplied (loud failure)", async () => {
  // An empty `template` is almost always a misconfiguration —
  // typically an unresolved caller-side ${VAR} or an empty default
  // fallback. Treat it the same as "neither supplied" so the failure
  // is the loud XOR-validation error, not a silent empty render.
  await assertRejects(
    () => main(stubSdk({ template: "" })),
    Error,
    "either template or template_path must be supplied",
  );
});

Deno.test("main: missing template param throws loud XOR-validation error", async () => {
  // No 'template' or 'template_path' key in the stub; both
  // params.get() calls return null. task.yaml no longer marks
  // template as required (since template_path is an alternative), so
  // the engine doesn't reject upstream — main() owns the loud-failure
  // contract via resolveTemplateBody.
  await assertRejects(
    () => main(stubSdk({})),
    Error,
    "either template or template_path must be supplied",
  );
});

Deno.test("main: env.get that throws PermissionDenied surfaces as unresolved placeholder", async () => {
  // Simulates the scoped --allow-env path where Deno.env.get(NAME) for
  // an undeclared NAME throws PermissionDenied (instead of returning
  // undefined). The user-facing error must be the loud
  // "unresolved placeholder" message — not a raw permission stack.
  const realGet = Deno.env.get.bind(Deno.env);
  Deno.env.get = ((name: string) => {
    if (name === "TPL_TEST_THROWS") {
      throw new Deno.errors.PermissionDenied(
        `Requires env access to "${name}"`,
      );
    }
    return realGet(name);
  }) as typeof Deno.env.get;
  try {
    await assertRejects(
      () =>
        main(stubSdk({
          template: "x=${TPL_TEST_THROWS}",
        })),
      Error,
      "unresolved placeholder: ${TPL_TEST_THROWS}",
    );
  } finally {
    Deno.env.get = realGet;
  }
});

Deno.test("main: prototype-shaped placeholder also fails loud", async () => {
  // The placeholder regex doesn't filter out names like "__proto__",
  // but the null-prototype env map + hasOwnProperty guard mean lookups
  // through it can't succeed — so the failure mode is the loud
  // "unresolved placeholder" path, not a silent prototype hit.
  await assertRejects(
    () => main(stubSdk({ template: "${__proto__}" })),
    Error,
    "unresolved placeholder: ${__proto__}",
  );
});

// --- resolveTemplateBody (XOR + file IO) ---

Deno.test("resolveTemplateBody: returns inline template verbatim", async () => {
  const out = await resolveTemplateBody("hello ${X}", null);
  assertEquals(out, "hello ${X}");
});

Deno.test("resolveTemplateBody: rejects both template and template_path", async () => {
  await assertRejects(
    () => resolveTemplateBody("inline", "/tmp/whatever"),
    Error,
    "both template and template_path supplied; exactly one is required",
  );
});

Deno.test("resolveTemplateBody: rejects neither template nor template_path", async () => {
  await assertRejects(
    () => resolveTemplateBody(null, null),
    Error,
    "either template or template_path must be supplied",
  );
});

Deno.test("resolveTemplateBody: empty-string template treated as not supplied", async () => {
  // Empty value is almost always a misconfiguration (unresolved
  // caller-side ${VAR}, empty default fallback) — fall through to
  // the XOR error instead of returning an empty render.
  await assertRejects(
    () => resolveTemplateBody("", ""),
    Error,
    "either template or template_path must be supplied",
  );
});

Deno.test("resolveTemplateBody: reads file via injected readFile", async () => {
  const out = await resolveTemplateBody(
    null,
    "/fake/path.tpl",
    (p) => {
      assertEquals(p, "/fake/path.tpl");
      return Promise.resolve("body from file ${X}");
    },
  );
  assertEquals(out, "body from file ${X}");
});

Deno.test("resolveTemplateBody: wraps readFile errors with path context", async () => {
  await assertRejects(
    () =>
      resolveTemplateBody(
        null,
        "/missing/path.tpl",
        () =>
          Promise.reject(
            new Deno.errors.NotFound("No such file or directory"),
          ),
      ),
    Error,
    "reading template_path /missing/path.tpl: No such file or directory",
  );
});

// --- main() + template_path end-to-end (real Deno.readTextFile) ---

Deno.test("main: template_path reads file then renders", async () => {
  // Write a temp file with a placeholder; set the corresponding env
  // var; invoke main() with template_path pointing at the file.
  const tmp = await Deno.makeTempFile({
    prefix: "buildin-template-test-",
    suffix: ".tpl",
  });
  try {
    await Deno.writeTextFile(tmp, "greeting: ${TPL_TEST_GREETING}");
    await withEnv({ TPL_TEST_GREETING: "hello" }, async () => {
      const out = await main(stubSdk({ template_path: tmp }));
      assertEquals(out, "greeting: hello");
    });
  } finally {
    await Deno.remove(tmp).catch(() => {});
  }
});

Deno.test("main: rejects both template and template_path", async () => {
  await assertRejects(
    () =>
      main(stubSdk({
        template: "inline body",
        template_path: "/tmp/whatever.tpl",
      })),
    Error,
    "exactly one",
  );
});

Deno.test("main: rejects neither template nor template_path", async () => {
  await assertRejects(
    () => main(stubSdk({})),
    Error,
    "either",
  );
});

Deno.test("main: template_path missing file produces loud wrapped error", async () => {
  // Build a path that we know doesn't exist. mkdtemp + immediate
  // remove keeps the parent dir valid (no surprise ENOTDIR) but
  // guarantees ENOENT on the target file.
  const dir = await Deno.makeTempDir({ prefix: "buildin-template-test-" });
  const bogus = `${dir}/does-not-exist.tpl`;
  try {
    await assertRejects(
      () => main(stubSdk({ template_path: bogus })),
      Error,
      `reading template_path ${bogus}:`,
    );
  } finally {
    await Deno.remove(dir, { recursive: true }).catch(() => {});
  }
});

// runningAsRoot returns true when this Deno process can read uid 0,
// false otherwise (including the common case where --allow-sys=uid is
// not granted and Deno.uid() throws NotCapable). chmod 0 is a no-op
// for root, so the permission-denied test below uses this to skip
// rather than emit a false pass.
function runningAsRoot(): boolean {
  try {
    return Deno.uid?.() === 0;
  } catch {
    return false;
  }
}

Deno.test({
  name: "main: template_path permission-denied produces loud wrapped error",
  // chmod 0 is meaningless on Windows; root on Linux can bypass mode bits.
  // Skip on Windows and when running as uid 0 to avoid a flaky false-pass.
  ignore: Deno.build.os === "windows" || runningAsRoot(),
  async fn() {
    const dir = await Deno.makeTempDir({ prefix: "buildin-template-test-" });
    const path = `${dir}/unreadable.tpl`;
    try {
      await Deno.writeTextFile(path, "shouldn't be reachable");
      await Deno.chmod(path, 0o000);
      await assertRejects(
        () => main(stubSdk({ template_path: path })),
        Error,
        `reading template_path ${path}:`,
      );
    } finally {
      // Restore mode so the cleanup can remove the file.
      await Deno.chmod(path, 0o600).catch(() => {});
      await Deno.remove(dir, { recursive: true }).catch(() => {});
    }
  },
});
