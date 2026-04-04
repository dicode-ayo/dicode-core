export default async function main({ log }) {
  const now = new Date().toISOString();
  await log.info(`Hello from js NEW ${now}`);
  return { message: `Hello from js NEW ${now}` };
}
