import type { DicodeSdk } from "../../sdk.ts";

// A "new project" wizard that pauses for input three times. Each pause names the
// step that handles the answer via suspend({ to }); the runner dispatches the
// matching entry in `steps` on resume, so there is no hand-rolled step switch and
// no `if (state)` branching. `state` carries earlier answers forward — the run is
// re-run from the top on every resume, so nothing lives in memory between pauses.

export default async function main({ dicode }: DicodeSdk) {
  await dicode.suspend({
    to: "chooseFramework",
    schema: {
      type: "object",
      title: "New project",
      description: "Let's scaffold a project. What should we call it?",
      properties: {
        project_name: { type: "string", title: "Project name" },
      },
      required: ["project_name"],
    },
  });
  // Unreachable — suspend() never returns.
}

export const steps = {
  // The project name arrives on ctx.input; ask for a framework and carry the
  // name forward in state so the confirm step can echo it back.
  async chooseFramework({ dicode, input }: DicodeSdk) {
    const project = (input as { project_name: string }).project_name;
    await dicode.suspend({
      to: "confirm",
      state: { project },
      schema: {
        type: "object",
        title: `Framework for ${project}`,
        properties: {
          framework: {
            type: "string",
            title: "Framework",
            enum: ["deno", "node", "bun"],
          },
        },
        required: ["framework"],
      },
    });
  },

  // Both prior answers are in hand (name in state, framework in input); ask for a
  // final confirmation, carrying name + framework forward.
  async confirm({ dicode, input, state }: DicodeSdk) {
    const project = (state as { project: string }).project;
    const framework = (input as { framework: string }).framework;
    await dicode.suspend({
      to: "summarize",
      state: { project, framework },
      schema: {
        type: "object",
        title: "Confirm",
        description: `Create ${project} (${framework})?`,
        properties: {
          confirmed: { type: "boolean", title: "Create the project?" },
        },
        required: ["confirmed"],
      },
    });
  },

  // Terminal step: return the collected summary.
  summarize({ input, state }: DicodeSdk) {
    const { project, framework } = state as { project: string; framework: string };
    const confirmed = (input as { confirmed: boolean }).confirmed;
    return { project, framework, confirmed };
  },
};
