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

export async function resume({ input, params }) {
  // api_key rides along only when the fire-time caller passed one; echoing
  // it back proves the continuation got the real value, not the redaction
  // placeholder.
  return { created: input?.project_name, api_key: await params.get('api_key') };
}
