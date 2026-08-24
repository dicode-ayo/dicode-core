import type { DicodeSdk } from "../../sdk.ts";

// The boilerplate CreateTask scaffolds. A directory still holding exactly this
// is a directory no agent has written to, whatever the reply says.
const SCAFFOLD_BODY = 'export default async function main({ dicode }) {\n  console.log("Hello from " + dicode.task_id);\n}\n';

// The caller's "I could not resolve a directory" value. Checking is skipped,
// not failed: an unevaluated post-condition must never read as a negative one.
const UNRESOLVED = "unknown";

export class VerificationFailed extends Error {}

async function readIfPresent(path: string): Promise<string | null> {
  try {
    return await Deno.readTextFile(path);
  } catch {
    return null;
  }
}

// verify answers one question: does this directory hold a task the agent
// wrote? It never consults the agent's own account of what it did.
export async function verify(taskDir: string): Promise<string> {
  const yaml = await readIfPresent(`${taskDir}/task.yaml`);
  if (yaml === null) {
    throw new VerificationFailed(
      `no task.yaml in ${taskDir} — the turn wrote no task there`,
    );
  }
  // Deliberately a shape check, not a YAML parse: the daemon's Go parser is
  // the authority on whether this file is valid, and re-implementing its
  // decisions here would drift from it. What matters is that a task is
  // declared at all.
  if (!/^\s*kind:\s*Task\s*$/m.test(yaml)) {
    throw new VerificationFailed(
      `task.yaml in ${taskDir} declares no 'kind: Task' — the turn left it unusable`,
    );
  }

  const bodies: string[] = [];
  for (const name of ["task.ts", "task.js", "task.py"]) {
    const body = await readIfPresent(`${taskDir}/${name}`);
    if (body !== null) bodies.push(body);
  }
  if (bodies.length === 0) {
    throw new VerificationFailed(
      `no task.ts, task.js or task.py in ${taskDir} — the turn wrote a manifest with nothing to run`,
    );
  }
  if (bodies.every((b) => b === SCAFFOLD_BODY)) {
    throw new VerificationFailed(
      `${taskDir} still holds only the scaffold — the turn changed nothing on disk, whatever its reply claims`,
    );
  }
  return `verified: ${taskDir} holds a task`;
}

export default async function main({ params }: DicodeSdk) {
  const taskDir = await params.get("task_dir") ?? "";
  const reply = await params.get("reply") ?? "";
  const session_id = await params.get("session_id") ?? "";

  // The response shape is the AI turn's, not this task's: as the pipeline's
  // terminal stage, whatever is returned here becomes the turn's answer to
  // the control plane.
  const response = { reply, session_id };

  if (taskDir === UNRESOLVED || taskDir === "") {
    console.warn(
      "verify-task-written: no task directory to check; the turn's post-condition was not evaluated",
    );
    return response;
  }

  // Throwing fails this run, which fails the pipeline, which is what stops a
  // turn that wrote nothing from settling as a success.
  console.log(await verify(taskDir));
  return response;
}
