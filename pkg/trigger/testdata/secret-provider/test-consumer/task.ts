export default async function main({ output }: any) {
  const pg = Deno.env.get("PG_URL") ?? "";
  const r  = Deno.env.get("REDIS_URL") ?? "";
  await output.text("PG_URL=" + pg + " REDIS_URL=" + r);
}
