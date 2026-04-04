
// Demonstrates calling another task via dicode.run_task().
// The caller needs permissions.dicode.tasks: [examples/notifications].

export default async function main({ params, log, dicode }: DicodeSdk) {
  const title    = (await params.get("title"))  ?? "Hellow";
  const message  = (await params.get("message")) ?? "World";
  const priority = (await params.get("priority")) ?? "default";
  const tags     = (await params.get("tags"))     ?? "";



  console.log("alert: dispatching notification via examples/notifications", { title, priority });

  const result = await dicode.run_task("examples/notifications", {
    title,
    body: message,
    priority,
    tags,
  });

  console.log("alert: notification run queued", { result });
  return { dispatched: true, title, priority };
}
