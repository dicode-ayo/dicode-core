import OpenAI from "npm:openai"

// input is provided by the on_failure_chain mechanism:
// { taskID, runID, status, output }
const { taskID, runID } = input as { taskID: string; runID: string }

if (!taskID || !runID) {
  log.warn("failure-monitor: missing taskID or runID in input", input)
  return { error: "missing input" }
}

log.info(`diagnosing failure: task=${taskID} run=${runID}`)

// Fetch recent runs to get logs
const runs = await dicode.get_runs(taskID, { limit: 5 })
const failedRun = runs.find((r: any) => r.ID === runID || r.id === runID) ?? runs[0]

const logs = (failedRun?.Logs ?? failedRun?.logs ?? [])
  .map((l: any) => `[${l.level ?? l.Level}] ${l.message ?? l.Message}`)
  .join("\n")

if (!logs) {
  log.warn("failure-monitor: no logs found for run", { runID })
}

// Use configured AI provider to diagnose
const aiCfg = await dicode.get_config("ai")
const client = new OpenAI({
  baseURL: aiCfg.baseURL || undefined,
  apiKey: aiCfg.apiKey || "ollama",
})

const response = await client.chat.completions.create({
  model: aiCfg.model,
  messages: [{
    role: "system",
    content: "You are a concise DevOps assistant. Diagnose task failures in 2-3 sentences.",
  }, {
    role: "user",
    content: `Task "${taskID}" failed (run ${runID}).\n\nLogs:\n${logs || "(no logs available)"}\n\nWhat went wrong and how can it be fixed?`,
  }],
})

const diagnosis = response.choices[0].message.content ?? "(no diagnosis)"
log.info("diagnosis complete", { taskID, diagnosis })

output.text(`Task: ${taskID}\nRun: ${runID}\n\n${diagnosis}`)
return { taskID, runID, diagnosis }
