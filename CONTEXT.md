# dicode

A GitOps task orchestrator: it watches sources of task definitions and runs them on
schedules, webhooks, or demand. This glossary fixes the vocabulary — what each term means
and which near-synonyms to avoid.

## Tasks and sources

**Task**:
A unit of automation: its configuration and the code it runs, versioned together.
_Avoid_: job, script, workflow

**Source**:
A place tasks are watched from, and the unit at which trust is declared.
_Avoid_: repo, folder, provider

**Taskset**:
A declared collection of sources and the overrides applied to what they contain.
_Avoid_: manifest, bundle, catalogue

**Override**:
A field on a task rewritten from outside that task's own definition. Overrides make a task
different from what its source alone would say it is.
_Avoid_: patch, customisation

**Builtin**:
A task shipped with dicode itself rather than supplied by an operator. Builtins are exempt
from approval.
_Avoid_: system task, internal task

**Content Hash**:
The identity of a task's current definition, across everything that determines what it
would do — including overrides. A task changes when its content hash changes.
_Avoid_: fingerprint, digest, version

## Execution

**Trigger**:
A condition that causes a task to run.
_Avoid_: schedule, hook, event

**Arm**:
To make a task's triggers live. An unarmed task is known and visible but cannot fire.
_Avoid_: enable, activate, deploy

**Run**:
One execution of one task, with its own logs, inputs, and outcome.
_Avoid_: execution, invocation, instance

## Approval

**Approval Gate**:
The mechanism that holds a changed task unarmed until an operator accepts it.
_Avoid_: review gate, security gate, quarantine

**Deploy Guard**:
What the approval gate *is* — the checkpoint between a change landing and that change
running. Named to distinguish it from an adversarial review: it assumes the author was
already authorised, and exists so a human sees the change before it fires.
_Avoid_: security review, audit, code review

**Pending**:
The state of a task whose current definition has not been approved. A pending task is
registered and visible but unarmed.
_Avoid_: unapproved, blocked, quarantined, held

**Review Surface**:
What an operator is shown in order to decide whether to arm a pending task.
_Avoid_: diff view, approval screen, review screen

**End State**:
The resolved task as it will run — what the review surface shows. End state describes the
task itself, so it requires no earlier version to compare against.
_Avoid_: preview, snapshot, diff, desired state

**What Moved**:
The secondary account of how the task got from its last approved version to this one.
Always secondary: when it cannot be determined, the review surface still shows end state.
_Avoid_: diff, changes, delta

**Approval Record**:
The durable statement that a specific version of a task was approved, by whom, and when.
_Avoid_: lock entry, approval state, trust record

**Trusted Source**:
A source whose tasks arm without an operator approving each change.
_Avoid_: allowlisted source, whitelisted source
