export default async function main({ params }) {
  return {
    greeting: await params.get("greeting"),
    api_token: await params.get("api_token"),
  };
}
