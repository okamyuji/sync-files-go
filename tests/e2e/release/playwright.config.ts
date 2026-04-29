// release 専用設定。BASE_URL は staging / prod の HTTPS エンドポイントを指す。
import { defineConfig, devices } from '@playwright/test';

const baseURL = process.env.BASE_URL ?? 'https://sync-staging.example.com';

export default defineConfig({
  testDir: '.',
  timeout: 60_000,
  expect: { timeout: 10_000 },
  fullyParallel: false,
  retries: process.env.CI ? 1 : 0,
  workers: 1,
  reporter: process.env.CI ? [['github'], ['html']] : 'list',
  use: {
    baseURL,
    trace: 'retain-on-failure',
    video: 'retain-on-failure',
    screenshot: 'only-on-failure',
    ignoreHTTPSErrors: !!process.env.ALLOW_INSECURE,
  },
  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
    // Phase 6 のリリース最終ゲートで Webkit / Firefox も足す
    // { name: 'webkit',   use: { ...devices['Desktop Safari'] } },
    // { name: 'firefox',  use: { ...devices['Desktop Firefox'] } },
  ],
});
