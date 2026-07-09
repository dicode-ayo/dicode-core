/**
 * cli-suspend.spec.ts
 *
 * End-to-end coverage for the `dicode` CLI against a suspending task.
 *
 * The webui surface is covered by suspend-resume.spec.ts. This drives the real
 * binary, because the CLI and the engine disagreed about what a suspended run
 * means and nothing caught it: `WaitRun` was changed to follow the resume chain
 * (right for dicode.run_task), the CLI's follow loop shares that waiter and
 * needs to observe `suspended` instead, and `dicode run` hung forever on any
 * task that suspends. Every unit test stayed green.
 *
 * Only non-TTY paths are exercised here. With no TTY, `dicode run` takes the
 * one-shot path unless --field answers can auto-advance the wizard — both of
 * which are exactly the behaviours that broke.
 */

import { test, expect } from '@playwright/test';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { BINARY } from './helpers/dicode-server';

const execFileAsync = promisify(execFile);

const TASK_ID = 'e2e-tests/suspend-wizard';
// Well under Playwright's per-test timeout: a hang must fail as a hang, not as
// a suite timeout with no output.
const CLI_TIMEOUT_MS = 20_000;

/** Run the CLI against the e2e daemon, with stdin closed so no TTY is detected. */
async function dicode(args: string[]): Promise<{ stdout: string; stderr: string }> {
  const dataDir = process.env.DICODE_E2E_TEMP_DIR;
  expect(dataDir, 'DICODE_E2E_TEMP_DIR must be set by the e2e daemon helper').toBeTruthy();

  return execFileAsync(BINARY, args, {
    env: { ...process.env, DICODE_DATA_DIR: dataDir! },
    timeout: CLI_TIMEOUT_MS,
    // The CLI aborts the daemon-autostart path when it cannot reach a terminal.
    windowsHide: true,
  });
}

test.describe('suspend/resume cli', () => {
  test.setTimeout(90_000);

  // The regression: `dicode run` blocked forever instead of printing the
  // suspended run id, because it waited on a run that was waiting on it.
  test('run --non-interactive prints the suspended run id and exits', async () => {
    const { stdout } = await dicode(['run', TASK_ID, '--non-interactive']);

    expect(stdout).toContain('suspended');
    // The id is what makes the one-shot path scriptable — `dicode resume <id>`.
    const runId = stdout.match(/[0-9a-f-]{36}/)?.[0];
    expect(runId, `no run id in CLI output:\n${stdout}`).toBeTruthy();

    // And it is genuinely resumable from that id.
    const { stdout: resumed } = await dicode(['resume', runId!, 'project_name=cli-e2e']);
    expect(resumed).toContain('resumed');
  });

  // Pre-supplied answers must auto-advance the wizard with no TTY and no prompt.
  test('run --field auto-advances the wizard to success', async () => {
    const { stdout } = await dicode([
      'run',
      TASK_ID,
      '--field',
      'project_name=cli-prefill',
      '--non-interactive',
    ]);

    expect(stdout).toContain('success');
    // The fixture's resume handler echoes the submitted value back as the result.
    expect(stdout).toContain('cli-prefill');
  });

  // `dicode resume` with no args lists suspended runs — the discovery surface a
  // headless operator uses after the one-shot path prints an id.
  test('resume with no args lists the suspended run', async () => {
    const { stdout: runOut } = await dicode(['run', TASK_ID, '--non-interactive']);
    const runId = runOut.match(/[0-9a-f-]{36}/)?.[0];
    expect(runId).toBeTruthy();

    const { stdout: list } = await dicode(['resume']);
    expect(list).toContain(runId!);
    expect(list).toContain(TASK_ID);
    // The field the run is waiting on is surfaced so the operator knows what to send.
    expect(list).toContain('project_name');
  });
});
