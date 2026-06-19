import { assertEquals } from "https://deno.land/std@0.224.0/assert/mod.ts";
import main from "./task.ts";

// Stub `gh` by setting PATH to a temp dir containing a wrapper that prints
// a known URL. The task runs gh with the project's working dir = clone path.
Deno.test("git-pr: returns ok with url on gh success", async () => {
    // Create a stub gh script.
    const tmp = await Deno.makeTempDir();
    const ghPath = `${tmp}/gh`;
    await Deno.writeTextFile(ghPath, `#!/bin/sh
echo "https://github.com/example/repo/pull/123"
`);
    await Deno.chmod(ghPath, 0o755);

    // Stub the dicode shim's params/return; call main.
    const origPath = Deno.env.get("PATH") ?? "";
    Deno.env.set("PATH", `${tmp}:${origPath}`);
    Deno.env.set("GH_TOKEN", "stub-token");

    const dataDir = await Deno.makeTempDir();
    const cloneRoot = `${dataDir}/dev-clones/user-tasks`;
    const cloneDir = `${cloneRoot}/run-123`;
    await Deno.mkdir(cloneDir, { recursive: true });
    Deno.env.set("DICODE_DATA_DIR", dataDir);

    try {
        const result = await main({
            params: new Map(Object.entries({
                source_id: "user-tasks",
                branch: "fix/abc",
                base: "main",
                title: "auto-fix",
                body: "",
                clone_path: cloneDir,
            })),
        } as any);
        assertEquals(result.ok, true);
        assertEquals(result.url, "https://github.com/example/repo/pull/123");
    } finally {
        Deno.env.set("PATH", origPath);
    }
});

Deno.test("git-pr: rejects path-traversal source_id", async () => {
    Deno.env.set("GH_TOKEN", "stub-token");
    Deno.env.set("DICODE_DATA_DIR", await Deno.makeTempDir());
    const result = await main({
        params: new Map(Object.entries({
            source_id: "../../etc",
            branch: "fix/abc",
            base: "main",
            title: "x",
            body: "",
        })),
    } as any);
    assertEquals(result.ok, false);
    if (!String(result.error ?? "").includes("invalid characters")) {
        throw new Error(`expected invalid-characters error, got ${result.error}`);
    }
});

Deno.test("git-pr: refuses to run without GH_TOKEN", async () => {
    Deno.env.delete("GH_TOKEN");
    Deno.env.set("DICODE_DATA_DIR", await Deno.makeTempDir());
    const result = await main({
        params: new Map(Object.entries({
            source_id: "user-tasks",
            branch: "fix/abc",
            base: "main",
            title: "x",
            body: "",
        })),
    } as any);
    assertEquals(result.ok, false);
    if (!String(result.error ?? "").includes("GH_TOKEN")) {
        throw new Error(`expected GH_TOKEN error, got ${result.error}`);
    }
});

Deno.test("git-pr: rejects clone_path outside dev-clones", async () => {
    Deno.env.set("GH_TOKEN", "stub-token");
    const dataDir = await Deno.makeTempDir();
    Deno.env.set("DICODE_DATA_DIR", dataDir);
    const result = await main({
        params: new Map(Object.entries({
            source_id: "user-tasks",
            branch: "fix/abc",
            base: "main",
            title: "x",
            body: "",
            clone_path: "/etc/passwd",
        })),
    } as any);
    assertEquals(result.ok, false);
    if (!String(result.error ?? "").includes("clone_path must be rooted")) {
        throw new Error(`expected clone_path-rooted error, got ${result.error}`);
    }
});

Deno.test("git-pr: returns ok=false when gh fails", async () => {
    const tmp = await Deno.makeTempDir();
    const ghPath = `${tmp}/gh`;
    await Deno.writeTextFile(ghPath, `#!/bin/sh
echo "gh: permission denied" >&2
exit 1
`);
    await Deno.chmod(ghPath, 0o755);

    const origPath = Deno.env.get("PATH") ?? "";
    Deno.env.set("PATH", `${tmp}:${origPath}`);

    const dataDir = await Deno.makeTempDir();
    const cloneRoot = `${dataDir}/dev-clones/user-tasks`;
    const cloneDir = `${cloneRoot}/run-456`;
    await Deno.mkdir(cloneDir, { recursive: true });
    Deno.env.set("DICODE_DATA_DIR", dataDir);

    try {
        const result = await main({
            params: new Map(Object.entries({
                source_id: "user-tasks", branch: "fix/abc", base: "main", title: "x", body: "",
                clone_path: cloneDir,
            })),
        } as any);
        assertEquals(result.ok, false);
    } finally {
        Deno.env.set("PATH", origPath);
    }
});

Deno.test("git-pr: hard-fails when clone_path is omitted", async () => {
    Deno.env.set("GH_TOKEN", "stub-token");
    Deno.env.set("DICODE_DATA_DIR", await Deno.makeTempDir());
    const result = await main({
        params: new Map(Object.entries({
            source_id: "user-tasks",
            branch: "fix/abc",
            base: "main",
            title: "x",
            body: "",
            // clone_path deliberately omitted — must hard-fail
        })),
    } as any);
    assertEquals(result.ok, false);
    if (!String(result.error ?? "").includes("clone_path is required")) {
        throw new Error(`expected clone_path-required error, got ${result.error}`);
    }
});
