/**
 * global-teardown.ts — Playwright global teardown entry point.
 * Stops the dicode process and cleans up temp dirs.
 */
import { teardown } from './dicode-server';

export default teardown;
