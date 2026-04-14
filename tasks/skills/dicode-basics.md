---
name: dicode-basics
description: Core concepts an agent should know about dicode — tasks, triggers, KV, and the relationship between tasks and the tools it can call.
---

# dicode basics

dicode is a local task runtime. Users write small TypeScript (or Python, Docker)
programs called **tasks** that get triggered by webhooks, cron, manual runs, or
chains from other tasks. You, the agent, are running **inside** a dicode task
yourself — you can see and call other tasks through `dicode.run_task()`.

## Key concepts

- **Task**: a single program with a `task.yaml` (metadata, triggers, permissions)
  and a `task.ts` (the code). Tasks are identified by ids like
  `buildin/ai-agent` or `examples/github-webhook`.
- **Trigger**: how a task is started — `webhook`, `cron`, `manual`, `chain`, or
  `daemon`. Each task has exactly one trigger type.
- **Params**: typed inputs declared in `task.yaml`. Webhook calls, cron
  invocations, and other tasks pass values for these.
- **KV**: per-task key-value store. Each task has an isolated namespace; data
  written by task A is not visible to task B.
- **Permissions**: every task declares what it's allowed to do (read env vars,
  hit the network, call other tasks). Denied by default — explicit allowlist.

## How to be a useful agent in this environment

- **Tools are tasks.** When you call a tool, you're running another dicode task
  with specific params. The tool's "description" and "parameters" come straight
  from its task.yaml. Read them carefully before calling.
- **Prefer tools over hand-waving.** If the user asks about data that a
  configured task can fetch, call the tool. Don't make up results.
- **Tool results are JSON.** The value returned by `dicode.run_task()` is the
  other task's `return` value, serialized. If it's empty, the other task
  probably used `output.html()` or `output.text()` instead of returning —
  surface that to the user rather than pretending you got data.
- **Be honest about limits.** You cannot read files, write secrets, or make
  arbitrary HTTP requests unless a tool lets you. If the user asks for
  something no tool exposes, say so clearly.
- **Respect the user's time.** Dicode runs on their laptop. Long tool loops
  burn local resources. Favor small, accurate answers over chains of
  exploratory calls.
