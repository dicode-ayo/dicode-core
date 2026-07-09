import type { DicodeSdk } from "../../sdk.ts";

// A branching deploy wizard. Two ideas it demonstrates beyond the linear
// suspend-wizard:
//   1. Steps do real async work between pauses — the helpers below stand in for
//      API calls / builds / scans (a delay + progress logs make the work visible
//      in the run's Logs panel).
//   2. The next step is chosen at runtime. `dicode.suspend({ to })` persists a
//      plain string, so `if/else` on an async result or on ctx.input picks the
//      branch — there is no static step graph.
//
// The run re-executes from the top on every resume (it is re-run, not frozen),
// so anything a later step needs is threaded through suspend({ state }) and read
// back as ctx.state. Nothing survives in a local variable across a pause.

async function delay(ms: number): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, ms));
}

async function gatherCandidate(): Promise<{ version: string; commits: number }> {
  console.log("deploy-wizard: scanning commits for the release candidate…");
  await delay(400);
  return { version: "1.4.0", commits: 7 };
}

async function runChecks(env: string): Promise<{ passed: boolean; coverage: number }> {
  console.log(`deploy-wizard: running tests + lint for ${env}…`);
  await delay(600);
  // Pretend prod runs a stricter suite that this candidate doesn't clear yet.
  const coverage = env === "prod" ? 71 : 88;
  return { passed: coverage >= 80, coverage };
}

async function deploy(env: string, version: string): Promise<{ url: string }> {
  console.log(`deploy-wizard: deploying ${version} to ${env}…`);
  await delay(800);
  return { url: `https://${env}.example.com/${version}` };
}

// The deploy-note step is reached from three branches (checks passed, override,
// prod confirmed); factored out so each branch just names it via `to`.
function deployNoteSchema(env: string, title: string) {
  return {
    to: "runDeploy",
    schema: {
      type: "object",
      title,
      description: `Deploying to ${env}.`,
      properties: { note: { type: "string", title: "Deploy note (optional)" } },
    },
  };
}

export default async function main({ dicode }: DicodeSdk) {
  const candidate = await gatherCandidate();
  await dicode.suspend({
    to: "pickEnv",
    state: { candidate },
    schema: {
      type: "object",
      title: `Release ${candidate.version}`,
      description: `${candidate.commits} commits since the last tag. Where should it go?`,
      properties: {
        env: { type: "string", title: "Environment", enum: ["dev", "staging", "prod"] },
      },
      required: ["env"],
    },
  });
}

export const steps = {
  async pickEnv({ dicode, input, state }: DicodeSdk) {
    const { candidate } = state as { candidate: { version: string } };
    const env = (input as { env: string }).env;

    const checks = await runChecks(env);

    // Branch on the async result: a failed gate asks whether to override.
    if (!checks.passed) {
      await dicode.suspend({
        to: "confirmOverride",
        state: { candidate, env, coverage: checks.coverage },
        schema: {
          type: "object",
          title: "Checks failed",
          description: `Coverage ${checks.coverage}% is below the 80% gate for ${env}. Override and deploy anyway?`,
          properties: { override: { type: "boolean", title: "Override the gate" } },
          required: ["override"],
        },
      });
    }

    // Checks passed: prod takes an extra confirmation, lower envs go straight on.
    if (env === "prod") {
      await dicode.suspend({
        to: "confirmProd",
        state: { candidate, env },
        schema: {
          type: "object",
          title: "Confirm production",
          description: `Ship ${candidate.version} to PROD?`,
          properties: { confirm: { type: "boolean", title: "Ship it" } },
          required: ["confirm"],
        },
      });
    }
    await dicode.suspend({ ...deployNoteSchema(env, `Deploy to ${env}`), state: { candidate, env } });
  },

  async confirmOverride({ dicode, input, state }: DicodeSdk) {
    const { candidate, env } = state as { candidate: { version: string }; env: string };
    if (!(input as { override: boolean }).override) {
      console.log("deploy-wizard: override declined — aborting");
      return { version: candidate.version, env, deployed: false, reason: "gate failed, override declined" };
    }
    // Overridden — prod still confirms; others go to the deploy note.
    if (env === "prod") {
      await dicode.suspend({
        to: "confirmProd",
        state: { candidate, env },
        schema: {
          type: "object",
          title: "Confirm production (overridden)",
          description: `Ship ${candidate.version} to PROD despite the failed gate?`,
          properties: { confirm: { type: "boolean", title: "Ship it" } },
          required: ["confirm"],
        },
      });
    }
    await dicode.suspend({ ...deployNoteSchema(env, `Deploy to ${env} (overridden)`), state: { candidate, env } });
  },

  async confirmProd({ dicode, input, state }: DicodeSdk) {
    const { candidate, env } = state as { candidate: { version: string }; env: string };
    if (!(input as { confirm: boolean }).confirm) {
      console.log("deploy-wizard: prod confirmation declined — aborting");
      return { version: candidate.version, env, deployed: false, reason: "prod confirmation declined" };
    }
    await dicode.suspend({ ...deployNoteSchema(env, "Deploy note"), state: { candidate, env } });
  },

  async runDeploy({ input, state }: DicodeSdk) {
    const { candidate, env } = state as { candidate: { version: string }; env: string };
    const note = (input as { note?: string }).note ?? "";
    const result = await deploy(env, candidate.version);
    console.log(`deploy-wizard: done — ${candidate.version} live at ${result.url}`);
    return { version: candidate.version, env, deployed: true, url: result.url, note };
  },
};
