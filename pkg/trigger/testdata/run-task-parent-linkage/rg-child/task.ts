export default async function main({ dicode }) {
  await dicode.set_group("conversation-7");
  return dicode.run_id;
}
