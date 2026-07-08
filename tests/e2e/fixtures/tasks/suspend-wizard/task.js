// First invocation suspends asking for a project name; the resume invocation
// echoes the submitted value back as the run's result (#512 e2e).
export default async function main({ dicode, resume_state, resume_input }) {
  if (!resume_state) {
    await dicode.suspend({
      state: { step: 'ask_name' },
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
  return { created: resume_input?.project_name };
}
