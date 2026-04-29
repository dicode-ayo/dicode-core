export default async function main() {
  console.log('will-fail: about to throw');
  throw new Error('intentional failure for e2e on_failure_chain test');
}
