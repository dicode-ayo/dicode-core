export default async function main({ input }) {
  console.log('webhook received ' + JSON.stringify(input));
  return { received: input };
}
