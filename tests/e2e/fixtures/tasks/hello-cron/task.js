export default async function main() {
  const time = new Date().toISOString();
  console.log('cron tick at ' + time);
  return { time };
}
