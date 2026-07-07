// First invocation suspends asking for a project name; the resume invocation
// echoes the submitted value back as the run's result (#95 e2e).
export default async function main({ dicode, resume_state, resume_input }) {
  if (!resume_state) {
    await dicode.suspend({
      state: { step: 'ask_name' },
      form: {
        title: "What's the project name?",
        fields: [
          { name: 'project_name', type: 'string', label: 'Name', required: true },
        ],
      },
    });
  }
  return { created: resume_input?.project_name };
}
