const { defineConfig } = require('@playwright/test');
const path = require('node:path');
require('./guard.cjs');
module.exports = defineConfig({
  testDir: __dirname,
  testMatch: 'real.spec.cjs',
  timeout: 25 * 60_000,
  expect: { timeout: 30_000 },
  workers: 1,
  retries: 0,
  outputDir: path.join(process.env.E2E_ARTIFACTS, 'browser'),
  reporter: [
    ['line'],
    ['html', { outputFolder: path.join(process.env.E2E_ARTIFACTS, 'report'), open: 'never' }],
    ['junit', { outputFile: path.join(process.env.E2E_ARTIFACTS, 'junit.xml') }],
    ['json', { outputFile: path.join(process.env.E2E_ARTIFACTS, 'results.json') }],
  ],
  use: {
    baseURL: 'https://127.0.0.1:29443',
    browserName: 'chromium',
    locale: 'en-US',
    viewport: { width: 1440, height: 1000 },
    permissions: ['clipboard-read', 'clipboard-write'],
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
    video: 'retain-on-failure',
    actionTimeout: 30_000,
    navigationTimeout: 45_000,
  },
});
