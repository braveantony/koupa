import { defineConfig } from '@playwright/test';

// The main app is started by run.sh (it needs a fresh database); the
// shutdown test spawns its own second instance on another port.
export default defineConfig({
  testDir: '.',
  testMatch: 'pages.spec.ts',
  workers: 1,
  fullyParallel: false,
  timeout: 30_000,
  use: { baseURL: process.env.BASE_URL ?? 'http://127.0.0.1:18080' },
  reporter: [['list']],
});
