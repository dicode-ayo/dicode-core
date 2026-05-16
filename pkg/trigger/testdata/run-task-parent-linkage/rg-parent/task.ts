export default async function main({ dicode }) {
  const result = await dicode.run_task("rg-child");
  return result?.runID ?? null;
}
