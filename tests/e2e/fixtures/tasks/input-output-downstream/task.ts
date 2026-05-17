// Test-only chain consumer for pkg/trigger/e2e_input_output_test.go.
//
// Reads `input.value` (delivered via chain.params after the engine's
// ${input.output} resolver runs) and writes it to ${MARKER_PATH}. The
// marker file content is the load-bearing assertion: the test compares
// it byte-for-byte against the expected substituted string.
//
// The SDK access pattern matches the verifier in
// e2e_template_preflight_pipeline_test.go — `input.<key>` directly,
// Deno.env.get for the marker path. We fail loud if the value is not a
// string so a regression in the resolver surfaces here rather than as a
// silent "wrote `[object Object]` to the marker".
export default async function main({ input }: { input: Record<string, unknown> }) {
  const value = input.value;
  if (typeof value !== "string") {
    throw new Error("input.value is not a string; input=" + JSON.stringify(input));
  }
  const markerPath = Deno.env.get("MARKER_PATH");
  if (!markerPath) throw new Error("MARKER_PATH not set");
  await Deno.writeTextFile(markerPath, value);
  return { written: markerPath };
}
