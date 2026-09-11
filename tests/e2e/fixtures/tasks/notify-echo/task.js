export default async function main({ params }) {
  const approveURL = (await params.get('approve_url')) ?? '';
  const taskID = (await params.get('task_id')) ?? '';
  return { approve_url: approveURL, task_id: taskID };
}
