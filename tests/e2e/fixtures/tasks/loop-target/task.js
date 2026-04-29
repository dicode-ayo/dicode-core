export default async function main() {
  console.log('loop-target: intentional failure to test chain-of-chains suppression');
  throw new Error('loop-target intentional failure');
}
