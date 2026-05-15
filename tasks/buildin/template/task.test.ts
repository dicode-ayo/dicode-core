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

Deno.test("main: empty template returns empty string", async () => {
  const out = await main(stubSdk({ template: "" }));
  assertEquals(out, "");
});

Deno.test("main: missing template param defaults to empty", async () => {
  // No 'template' key in the stub; params.get returns null;
  // task.ts coerces to "".
  const out = await main(stubSdk({}));
  assertEquals(out, "");
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
