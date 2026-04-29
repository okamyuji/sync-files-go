// smoke.spec.ts
//
// Phase 6 リリースゲート用の最小確認テスト。staging / prod の HTTPS エンドポイントに対して走らせる。
// 環境変数:
//   BASE_URL    https://sync-staging.example.com 等
//   ALLOW_INSECURE=1  自己署名証明書を許可
import { test, expect, request } from '@playwright/test';

const baseURL = process.env.BASE_URL ?? 'https://sync-staging.example.com';

test.describe('release smoke', () => {
  test('GET /healthz returns 200 ok', async ({ playwright }) => {
    const ctx = await playwright.request.newContext({ ignoreHTTPSErrors: !!process.env.ALLOW_INSECURE });
    const r = await ctx.get(`${baseURL}/healthz`);
    expect(r.status()).toBe(200);
    expect(await r.text()).toContain('ok');
    await ctx.dispose();
  });

  test('GET /readyz returns 200 (DB + storage reachable)', async ({ playwright }) => {
    const ctx = await playwright.request.newContext({ ignoreHTTPSErrors: !!process.env.ALLOW_INSECURE });
    const r = await ctx.get(`${baseURL}/readyz`);
    expect(r.status()).toBe(200);
    expect(await r.text()).toContain('ready');
    await ctx.dispose();
  });

  test('GET /login renders sign-in form', async ({ page }) => {
    await page.goto(`${baseURL}/login`);
    await expect(page).toHaveTitle(/サインイン/);
    await expect(page.locator('input[name="email"]')).toBeVisible();
    await expect(page.locator('input[name="password"]')).toBeVisible();
  });

  test('security headers present', async ({ playwright }) => {
    const ctx = await playwright.request.newContext({ ignoreHTTPSErrors: !!process.env.ALLOW_INSECURE });
    const r = await ctx.get(`${baseURL}/login`);
    const headers = r.headers();
    expect(headers['strict-transport-security']).toMatch(/max-age=\d+/);
    expect(headers['x-content-type-options']).toBe('nosniff');
    expect(headers['x-frame-options']).toBe('DENY');
    expect(headers['content-security-policy']).toMatch(/nonce-/);
    await ctx.dispose();
  });
});
