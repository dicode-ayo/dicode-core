export default async function main({ input }) {
  console.log('secure webhook received ' + JSON.stringify(input));
  return { received: input };
}
