export default async function main({ input }) {
  console.log('any-auth webhook received ' + JSON.stringify(input));
  return { received: input };
}
