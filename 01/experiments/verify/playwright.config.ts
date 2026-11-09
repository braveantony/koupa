import { defineConfig } from '@playwright/test';

// Protocol cross-check (plan A14): run by protocol.sh inside the pinned
// container mcr.microsoft.com/playwright:v1.49.1-noble, with
// experiments/browser/node_modules (@playwright/test 1.49.1) mounted next
// to this directory. Each test runs until protocol.sh creates STOP_FILE.
export default defineConfig({
  testDir: '.',
  testMatch: 'browser.spec.ts',
  workers: 1,
  fullyParallel: false,
  timeout: 15 * 60_000,
  outputDir: process.env.PW_OUTPUT_DIR ?? '/tmp/pw-results',
  reporter: [['list']],
});
