import { assertEquals, assertRejects, assertThrows } from "https://deno.land/std@0.224.0/assert/mod.ts";
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
  assertThrows(() => parseMode("rwx"), Error, "invalid mode");
});

Deno.test("mode_rejects_non_octal_digits", () => {
  assertThrows(() => parseMode("9999"), Error, "invalid mode");
});

Deno.test("mode_rejects_special_bits", () => {
  // 4-digit octal values whose first digit is non-zero carry
  // setuid/setgid/sticky bits. A secret-bearing config has no business
  // landing with mode 4755. The 3-digit regex (with optional leading
  // zero) blocks these while still accepting "0600"/"0644".
  assertThrows(() => parseMode("7777"), Error, "invalid mode");
  assertThrows(() => parseMode("4755"), Error, "invalid mode");
  assertThrows(() => parseMode("2755"), Error, "invalid mode");
  assertThrows(() => parseMode("1755"), Error, "invalid mode");
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

Deno.test("rejects_relative_path", async () => {
  await assertRejects(
    () => main(sdk({ content: "x", path: "foo/bar.txt", mode: "0600" })),
    Error,
    "must be absolute",
  );
});

Deno.test("rejects_dotdot_path", async () => {
  await assertRejects(
    () => main(sdk({ content: "x", path: "/tmp/foo/../bar.txt", mode: "0600" })),
    Error,
    "parent-directory segments not allowed",
  );
  await assertRejects(
    () => main(sdk({ content: "x", path: "/tmp/foo/..", mode: "0600" })),
    Error,
    "parent-directory segments not allowed",
  );
});

Deno.test("final_file_at_target_path_not_tmp", async () => {
  // Atomic write goes through a sibling tmp file. After main() returns,
  // the target path must exist and no .tmp.* sibling may be left behind.
  await withTmpDir(async (dir) => {
    const path = `${dir}/out.yaml`;
    const result = await main(sdk({ content: "data", path }));
    assertEquals(result, { path });
    assertEquals(await Deno.readTextFile(path), "data");

    const leftovers: string[] = [];
    for await (const entry of Deno.readDir(dir)) {
      leftovers.push(entry.name);
    }
    // Only the final file should be present — no `.tmp.<uuid>` siblings.
    assertEquals(leftovers, ["out.yaml"]);
  });
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
