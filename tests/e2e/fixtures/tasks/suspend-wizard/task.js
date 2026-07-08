// Two-function shape (#512): main suspends asking for a project name; the runner
// dispatches the exported `resume` on the continuation, which echoes the
// submitted value back as the run's result — no hand-rolled resume switch.
export default async function main({ dicode }) {
  await dicode.suspend({
    schema: {
      type: 'object',
      title: "What's the project name?",
      properties: {
        project_name: { type: 'string', title: 'Name' },
      },
      required: ['project_name'],
    },
  });
}

export async function resume({ input }) {
  return { created: input?.project_name };
}
