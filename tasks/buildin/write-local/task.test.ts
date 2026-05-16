import { assertEquals, assertRejects } from "https://deno.land/std@0.224.0/assert/mod.ts";
import main, { parseMode } from "./task.ts";

// Test helper — wraps the SDK shape main() expects.
function sdk(params: Record<string, string>) {
  return {
    params: {
      get: (k: string) =>
        Promise.resolve(params[k] ?? null),
    },
  } as never;
}

async function withTmpDir(fn: (dir: string) => Promise<void>): Promise<void> {
  const dir = await Deno.makeTempDir({ prefix: "write-local-test-" });
  try {
    await fn(dir);
  } finally {
    await Deno.remove(dir, { recursive: true });
  }
}

Deno.test("write_creates_file", async () => {
  await withTmpDir(async (dir) => {
    const path = `${dir}/out.txt`;
    const result = await main(sdk({ content: "hello\nworld\n", path, mode: "0600" }));
    assertEquals(result, { path });
    const written = await Deno.readTextFile(path);
    assertEquals(written, "hello\nworld\n");
    const stat = await Deno.stat(path);
    if (stat.mode !== null) {
      assertEquals(stat.mode & 0o777, 0o600);
    }
  });
});

Deno.test("write_overwrites", async () => {
  await withTmpDir(async (dir) => {
    const path = `${dir}/out.txt`;
    await Deno.writeTextFile(path, "old");
    const result = await main(sdk({ content: "new", path, mode: "0600" }));
    assertEquals(result, { path });
    assertEquals(await Deno.readTextFile(path), "new");
  });
});

Deno.test("write_creates_parent_dir", async () => {
  await withTmpDir(async (dir) => {
    const path = `${dir}/nested/sub/out.yaml`;
    const result = await main(sdk({ content: "y", path, mode: "0600" }));
    assertEquals(result, { path });
    assertEquals(await Deno.readTextFile(path), "y");
  });
});

Deno.test("mode_default_is_0600", async () => {
  await withTmpDir(async (dir) => {
    const path = `${dir}/out.txt`;
    await main(sdk({ content: "x", path }));
    const stat = await Deno.stat(path);
    if (stat.mode !== null) {
      assertEquals(stat.mode & 0o777, 0o600);
    }
  });
});

Deno.test("mode_accepts_0644", async () => {
  await withTmpDir(async (dir) => {
    const path = `${dir}/out.txt`;
    await main(sdk({ content: "x", path, mode: "0644" }));
    const stat = await Deno.stat(path);
    if (stat.mode !== null) {
      assertEquals(stat.mode & 0o777, 0o644);
    }
  });
});

Deno.test("mode_accepts_three_digit", () => {
  assertEquals(parseMode("600"), 0o600);
  assertEquals(parseMode("0600"), 0o600);
  assertEquals(parseMode("644"), 0o644);
});

Deno.test("mode_rejects_garbage", () => {
  try {
    parseMode("rwx");
    throw new Error("expected throw");
  } catch (e) {
    if (!(e instanceof Error) || !e.message.includes("invalid mode")) {
      throw new Error(`wrong error: ${e}`);
    }
  }
});

Deno.test("mode_rejects_non_octal_digits", () => {
  try {
    parseMode("9999");
    throw new Error("expected throw");
  } catch (e) {
    if (!(e instanceof Error) || !e.message.includes("invalid mode")) {
      throw new Error(`wrong error: ${e}`);
    }
  }
});

Deno.test("rejects_missing_content", async () => {
  await assertRejects(
    () => main(sdk({ path: "/tmp/x", mode: "0600" })),
    Error,
    "missing required param: content",
  );
});

Deno.test("rejects_missing_path", async () => {
  await assertRejects(
    () => main(sdk({ content: "x", mode: "0600" })),
    Error,
    "missing required param: path",
  );
});

Deno.test("rejects_path_with_nul", async () => {
  await assertRejects(
    () => main(sdk({ content: "x", path: "/tmp/foo\0bar", mode: "0600" })),
    Error,
    "invalid path",
  );
});

Deno.test("write_accepts_empty_content", async () => {
  await withTmpDir(async (dir) => {
    const path = `${dir}/empty.txt`;
    const result = await main(sdk({ content: "", path }));
    assertEquals(result, { path });
    const stat = await Deno.stat(path);
    assertEquals(stat.size, 0);
  });
});
