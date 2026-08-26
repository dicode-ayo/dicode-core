import { assertEquals, assertRejects } from "https://deno.land/std@0.224.0/assert/mod.ts";
import main, { verify, VerificationFailed } from "./task.ts";

const SCAFFOLD_BODY =
  'export default async function main({ dicode }: DicodeSdk) {\n  console.log("Hello from " + dicode.task_id);\n}\n';
// The pre-#741 task.js scaffold body (no DicodeSdk type annotation). A task
// directory scaffolded before that fix still holds this verbatim until an
// agent edits it, so it must keep failing verification too.
const LEGACY_SCAFFOLD_BODY =
  'export default async function main({ dicode }) {\n  console.log("Hello from " + dicode.task_id);\n}\n';
const REAL_YAML = "apiVersion: dicode/v1\nkind: Task\nname: zen\n";

function sdk(params: Record<string, string>) {
  return {
    params: { get: (k: string) => Promise.resolve(params[k] ?? null) },
  } as never;
}

async function withTaskDir(
  files: Record<string, string>,
  fn: (dir: string) => Promise<void>,
): Promise<void> {
  const dir = await Deno.makeTempDir({ prefix: "verify-task-written-test-" });
  try {
    for (const [name, body] of Object.entries(files)) {
      await Deno.writeTextFile(`${dir}/${name}`, body);
    }
    await fn(dir);
  } finally {
    await Deno.remove(dir, { recursive: true });
  }
}

Deno.test("untouched_scaffold_fails_however_confident_the_reply", async () => {
  await withTaskDir(
    { "task.yaml": REAL_YAML, "task.ts": SCAFFOLD_BODY },
    async (dir) => {
      const err = await assertRejects(
        () => main(sdk({ task_dir: dir, reply: "I wrote all three files." })),
        VerificationFailed,
      );
      assertEquals(err.message.includes("still holds only the scaffold"), true);
    },
  );
});

// A task directory scaffolded before #741 still holds the untyped task.js
// body verbatim until someone edits it — that untouched legacy scaffold must
// still fail verification, not silently pass because it no longer matches
// the current (typed, task.ts) scaffold string.
Deno.test("untouched_legacy_task_js_scaffold_still_fails", async () => {
  await withTaskDir(
    { "task.yaml": REAL_YAML, "task.js": LEGACY_SCAFFOLD_BODY },
    async (dir) => {
      const err = await assertRejects(
        () => main(sdk({ task_dir: dir, reply: "I wrote all three files." })),
        VerificationFailed,
      );
      assertEquals(err.message.includes("still holds only the scaffold"), true);
    },
  );
});

Deno.test("a_written_task_verifies", async () => {
  await withTaskDir(
    {
      "task.yaml": REAL_YAML,
      "task.ts": "export default async function main() { return 1 }\n",
    },
    async (dir) => {
      const out = await main(
        sdk({ task_dir: dir, reply: "done", session_id: "s-1" }),
      );
      assertEquals(out, { reply: "done", session_id: "s-1" });
    },
  );
});

Deno.test("missing_manifest_fails", async () => {
  await withTaskDir({}, async (dir) => {
    await assertRejects(() => verify(dir), VerificationFailed, "no task.yaml");
  });
});

Deno.test("manifest_without_kind_Task_fails", async () => {
  await withTaskDir(
    { "task.yaml": "apiVersion: dicode/v1\nname: nope\n", "task.ts": "x" },
    async (dir) => {
      await assertRejects(() => verify(dir), VerificationFailed, "kind: Task");
    },
  );
});

Deno.test("manifest_with_no_body_fails", async () => {
  await withTaskDir({ "task.yaml": REAL_YAML }, async (dir) => {
    await assertRejects(
      () => verify(dir),
      VerificationFailed,
      "nothing to run",
    );
  });
});

// An unresolved directory is not a failed check. Conflating "could not look"
// with "found nothing" is the bug this whole post-condition exists to avoid.
Deno.test("unresolved_directory_skips_rather_than_fails", async () => {
  const out = await main(
    sdk({ task_dir: "unknown", reply: "hi", session_id: "s-2" }),
  );
  assertEquals(out, { reply: "hi", session_id: "s-2" });
});

// The turn's answer travels through this stage untouched: it is the pipeline's
// terminal stage, so what it returns is what the control plane reads back.
Deno.test("passes_the_turns_reply_through_verbatim", async () => {
  await withTaskDir(
    { "task.yaml": REAL_YAML, "task.py": "print(1)\n" },
    async (dir) => {
      const out = await main(
        sdk({ task_dir: dir, reply: "multi\nline reply", session_id: "s-3" }),
      );
      assertEquals(out, { reply: "multi\nline reply", session_id: "s-3" });
    },
  );
});
