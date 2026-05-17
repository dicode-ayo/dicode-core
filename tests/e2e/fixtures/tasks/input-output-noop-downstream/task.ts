// Negative-case downstream for pkg/trigger/e2e_input_output_test.go.
//
// This task should NEVER actually execute under the test scenario it
// targets: the chained upstream returns a non-string, so the engine's
// ${input.output} resolver must short-circuit before dispatching this
// task body. If it DOES run, we drop a "ran" marker so the test can
// detect the contract violation.
export default async function main() {
  const markerPath = Deno.env.get("MARKER_PATH");
  if (!markerPath) throw new Error("MARKER_PATH not set");
  await Deno.writeTextFile(markerPath, "ran");
  return { error: "this task should not have fired" };
}
