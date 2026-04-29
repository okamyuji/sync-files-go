import { defineConfig, devices } from '@playwright/test';

/**
 * sync-files-go E2E 設定
 *
 * 前提:
 *   docker compose up（プロジェクトルートで `make compose-up && make db-migrate`）が起動済み。
 *   テストは独立したユーザを使うため並列実行可能だが、Phase 5 は安全優先で fullyParallel=false。
 *
 * 環境変数:
 *   BASE_URL  デフォルト http://localhost:8080
 *   CI        CI 上ではリトライ 2 回
 */
const baseURL = process.env.BASE_URL ?? 'http://localhost:8080';

export default defineConfig({
  testDir: './tests',
  timeout: 30_000,
  expect: { timeout: 5_000 },
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  workers: process.env.CI ? 1 : 1,
  reporter: process.env.CI ? [['github'], ['html']] : 'list',
  use: {
    baseURL,
    trace: 'on-first-retry',
    video: 'retain-on-failure',
    screenshot: 'only-on-failure',
    ignoreHTTPSErrors: true,
  },
  projects: [
    { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
    // Webkit / Firefox は Phase 6 のリリース前疎通テストで有効化する。
    // { name: 'webkit',   use: { ...devices['Desktop Safari'] } },
    // { name: 'firefox',  use: { ...devices['Desktop Firefox'] } },
  ],
});
