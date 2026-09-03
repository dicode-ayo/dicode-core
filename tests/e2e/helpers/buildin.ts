/**
 * buildin.ts — resolve the dicode-buildin taskset for the e2e suite.
 *
 * The dashboard SPA and the mcp / auth-providers / local-storage /
 * run-inputs-cleanup tasks live in dicode-ayo/dicode-buildin, but the suite
 * needs them on disk: the fixture taskset mounts five of them under the
 * `e2e-tests` namespace by absolute path, and the relay specs mount the whole
 * manifest under a `buildin` source. Letting the daemon resolve the git ref
 * itself would namespace every task `buildin/…` and break the `e2e-tests/…`
 * ids the specs assert on.
 *
 * So clone it here, into a gitignored cache the suite reuses across runs.
 *
 * BUILDIN_REF pins what is checked out. It tracks `main` until dicode-core#825
 * lands tag refs, at which point this becomes the tag the daemon ships with.
 * Set DICODE_E2E_BUILDIN_DIR to point at a local checkout instead — the escape
 * hatch for developing a task and its consuming test together.
 */
import { execFileSync } from 'child_process';
import * as fs from 'fs';
import * as path from 'path';

export const BUILDIN_URL = 'https://github.com/dicode-ayo/dicode-buildin';
export const BUILDIN_REF = 'main';

/**
 * Returns the directory holding a dicode-buildin checkout, cloning or
 * refreshing the cache as needed. Honours DICODE_E2E_BUILDIN_DIR.
 */
export function ensureBuildinCheckout(repoRoot: string): string {
  const override = process.env.DICODE_E2E_BUILDIN_DIR;
  if (override) {
    if (!fs.existsSync(path.join(override, 'taskset.yaml'))) {
      throw new Error(
        `DICODE_E2E_BUILDIN_DIR=${override} has no taskset.yaml — not a dicode-buildin checkout`,
      );
    }
    return override;
  }

  const dir = path.join(repoRoot, '.buildin-cache');
  const git = (args: string[], cwd: string) =>
    execFileSync('git', args, { cwd, stdio: 'pipe', encoding: 'utf8' });

  if (!fs.existsSync(path.join(dir, '.git'))) {
    fs.rmSync(dir, { recursive: true, force: true });
    git(['clone', '--depth', '1', '--branch', BUILDIN_REF, BUILDIN_URL, dir], repoRoot);
  } else {
    // Refresh in place. A pinned tag makes this a no-op; on a branch it picks
    // up what the daemon would pick up on its next reconcile.
    git(['fetch', '--depth', '1', 'origin', BUILDIN_REF], dir);
    git(['reset', '--hard', 'FETCH_HEAD'], dir);
  }

  const head = git(['rev-parse', '--short', 'HEAD'], dir).trim();
  console.log(`e2e: dicode-buildin @ ${BUILDIN_REF} (${head}) in ${dir}`);
  return dir;
}
