// deno-lint-ignore-file no-explicit-any

// SOURCE_ID_RE constrains source_id to a flat identifier — the same
// character class that pkg/taskset/branch_validate.go's ValidateRunID
// uses for clone-dir name components. Any path-traversal sequence
// (`..`, `/`, leading `-`) is rejected before we touch the filesystem.
const SOURCE_ID_RE = /^[A-Za-z0-9_][A-Za-z0-9_-]{0,63}$/;

export default async function main(opts: { params: Map<string, string> }) {
    const sourceID  = opts.params.get("source_id")  ?? "";
    const branch    = opts.params.get("branch")     ?? "";
    const base      = opts.params.get("base")       ?? "main";
    const title     = opts.params.get("title")      ?? "";
    const body      = opts.params.get("body")       ?? "";
    // clone_path is the explicit working directory passed by the caller
    // (the auto-fix skill knows this from the value it returned from
    // dicode.sources.set_dev_mode). Falling back to readDir under
    // dev-clones is a legacy path; new callers should always pass it.
    const clonePath = opts.params.get("clone_path") ?? "";

    if (!sourceID || !branch || !title) {
        return { ok: false, error: "source_id, branch, and title are required" };
    }
    if (!SOURCE_ID_RE.test(sourceID)) {
        return { ok: false, error: `source_id ${JSON.stringify(sourceID)} contains invalid characters; expected ^[A-Za-z0-9_][A-Za-z0-9_-]{0,63}$` };
    }

    // Refuse to invoke gh without an explicit token. Without this guard
    // the gh binary would fall back to an ambient `gh auth login` token
    // (potentially admin-scoped), leaking authorization beyond what the
    // task spec declares. permissions.env declares GH_TOKEN_AUTOFIX
    // mapped onto GH_TOKEN; if the operator hasn't set the secret we
    // refuse loudly rather than silently borrowing credentials.
    const ghToken = Deno.env.get("GH_TOKEN") ?? "";
    if (!ghToken) {
        return { ok: false, error: "GH_TOKEN is not set; refusing to invoke gh with ambient credentials. Configure the GH_TOKEN_AUTOFIX secret." };
    }

    const dataDir = Deno.env.get("DICODE_DATA_DIR") ??
                    `${Deno.env.get("HOME")}/.dicode`;
    const cloneRoot = `${dataDir}/dev-clones/${sourceID}`;

    let workdir: string;
    if (clonePath) {
        // Validate that the explicit path is rooted inside cloneRoot —
        // defense-in-depth even though the caller is normally trusted.
        if (!clonePath.startsWith(cloneRoot + "/") && clonePath !== cloneRoot) {
            return { ok: false, error: `clone_path must be rooted at ${cloneRoot}; got ${clonePath}` };
        }
        workdir = clonePath;
    } else {
        // Legacy fallback: scan dev-clones/<source_id>/* for the first
        // directory entry. Brittle if a previous run left an orphan
        // clone; warn loudly so the caller migrates.
        workdir = cloneRoot;
        let found = false;
        try {
            for await (const entry of Deno.readDir(cloneRoot)) {
                if (entry.isDirectory) {
                    workdir = `${cloneRoot}/${entry.name}`;
                    found = true;
                    break;
                }
            }
        } catch (e) {
            return { ok: false, error: `clone root unreadable: ${e}` };
        }
        if (!found) {
            return { ok: false, error: `no clone directory under ${cloneRoot}; pass clone_path explicitly` };
        }
        console.warn(`git-pr: clone_path not provided; picked ${workdir} via readDir scan. Pass clone_path explicitly to avoid orphan-clone races.`);
    }

    const cmd = new Deno.Command("gh", {
        args: ["pr", "create",
               "--title", title,
               "--body",  body,
               "--base",  base,
               "--head",  branch],
        cwd:    workdir,
        stdout: "piped",
        stderr: "piped",
    });
    const { success, stdout, stderr } = await cmd.output();
    const out = new TextDecoder().decode(stdout).trim();
    if (!success) {
        return { ok: false, error: new TextDecoder().decode(stderr).trim() };
    }
    return { ok: true, url: out };
}
