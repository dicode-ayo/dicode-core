export default async function main({ log }) {
  const now = new Date().toISOString();
  console.log(`Hello from js NEW ${now}`);
  return { message: `Hello from js NEW ${now}` };
}
