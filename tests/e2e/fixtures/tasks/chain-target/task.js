export default async function main({ input }) {
  console.log('chain-target received: ' + JSON.stringify(input));
  return { input };
}
