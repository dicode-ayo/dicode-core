// deno-lint-ignore-file no-explicit-any
export default async function main(opts: { params: Map<string, string> }) {
    const sourceID = opts.params.get("source_id") ?? "";
    const branch   = opts.params.get("branch")    ?? "";
    const base     = opts.params.get("base")      ?? "main";
    const title    = opts.params.get("title")     ?? "";
    const body     = opts.params.get("body")      ?? "";

    if (!sourceID || !branch || !title) {
        return { ok: false, error: "source_id, branch, and title are required" };
    }

    // Resolve the dev-clone working directory for this source.
    // The auto-fix flow already cloned to ${DATADIR}/dev-clones/<source_id>/<runID>.
    // The clone path for the active dev session is recorded by the engine in the
    // run's parent input under "_clone_path" (set by sources.set_dev_mode).
    // For this task, we accept the simpler contract: the caller passes a working
    // directory implicitly via Deno.cwd() OR we stat the dev-clones tree.
    const dataDir = Deno.env.get("DICODE_DATA_DIR") ??
                    `${Deno.env.get("HOME")}/.dicode`;
    const cloneRoot = `${dataDir}/dev-clones/${sourceID}`;
    let workdir = cloneRoot;
    try {
        for await (const entry of Deno.readDir(cloneRoot)) {
            if (entry.isDirectory) { workdir = `${cloneRoot}/${entry.name}`; break; }
        }
    } catch (e) {
        return { ok: false, error: `clone root unreadable: ${e}` };
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
