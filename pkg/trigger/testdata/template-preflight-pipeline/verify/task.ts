export default async function main({ input }) {
  const marker = input.marker;
  const output = input.output;
  if (typeof marker !== "string") throw new Error("missing marker: " + JSON.stringify(input));
  if (typeof output !== "string") throw new Error("missing output: " + JSON.stringify(input));
  const path = Deno.env.get("MARKER_PATH");
  if (!path) throw new Error("MARKER_PATH not set");
  await Deno.writeTextFile(path, marker + ":" + output);
  return output;
}
