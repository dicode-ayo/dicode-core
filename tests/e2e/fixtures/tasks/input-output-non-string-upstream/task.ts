// Test-only upstream that returns a JSON object (NOT a string) so
// pkg/trigger/e2e_input_output_test.go can pin the
// ${input.output} resolver's "non-string returns short-circuit the
// chain dispatch" contract end-to-end through the real Deno runtime.
export default async function main() {
  return { x: 1, y: "hello" };
}
