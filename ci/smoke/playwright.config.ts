import { defineConfig, devices } from '@playwright/test';

// The dashboard under test is tools/devpreview: the real web tier on in-memory
// stubs, auto-authenticated, with a fake streaming console. No Postgres, no
// gvmd. Playwright starts it itself (go run) and tears it down after.
const baseURL = 'http://127.0.0.1:8130';

export default defineConfig({
  testDir: '.',
  timeout: 30_000,
  expect: { timeout: 10_000 },
  reporter: 'list',
  forbidOnly: !!process.env.CI,
  use: { baseURL },
  webServer: {
    command: 'go run ./tools/devpreview',
    cwd: '../..',
    url: `${baseURL}/instances`,
    timeout: 120_000,
    reuseExistingServer: !process.env.CI,
  },
  projects: [
    { name: 'desktop', use: { ...devices['Desktop Chrome'] } },
    { name: 'mobile', use: { ...devices['Pixel 5'] } },
  ],
});
