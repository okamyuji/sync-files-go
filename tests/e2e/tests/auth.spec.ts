import { test, expect } from '@playwright/test';
import { signupAndLogin, uniqueEmail } from '../fixtures/auth';

test.describe('auth', () => {
  test('signup → login → home redirect', async ({ page, request, baseURL }) => {
    const url = baseURL ?? 'http://localhost:8080';

    // API 経由で signup + login し、ブラウザ側に Cookie を移す
    const ctx = await page.context();
    const auth = await signupAndLogin(request, url);
    const cookies = await request.storageState();
    await ctx.addCookies(cookies.cookies);

    await page.goto('/');
    await expect(page).toHaveTitle(/ホーム/);
    await expect(page.locator('h1.file-toolbar-title')).toContainText('ホーム');
  });

  test('login page renders with all required fields', async ({ page }) => {
    await page.goto('/login');
    await expect(page).toHaveTitle(/サインイン/);
    await expect(page.locator('input[name="email"]')).toBeVisible();
    await expect(page.locator('input[name="password"]')).toBeVisible();
    await expect(page.locator('input[name="totp"]')).toBeVisible();
    await expect(page.locator('button[type="submit"]')).toContainText('サインイン');
  });

  test('signup form: HTML5 validation blocks short password', async ({ page }) => {
    // minlength="8" 制約があるので 8 文字未満を入れて submit すると、ブラウザの組み込みバリデーションが発火する。
    // クライアント側の防御を確認するテスト（サーバ側のバリデーションは別途）。
    await page.goto('/signup');
    await page.fill('input[name="email"]', uniqueEmail('shortpw'));
    await page.fill('input[name="password"]', 'short');
    const isInvalid = await page.locator('input[name="password"]').evaluate(
      (el: HTMLInputElement) => !el.checkValidity(),
    );
    expect(isInvalid, 'password input must be marked invalid').toBe(true);
  });

  test('login fails with wrong credentials shows server error', async ({ page }) => {
    await page.goto('/login');
    await page.fill('input[name="email"]', uniqueEmail('nouser'));
    await page.fill('input[name="password"]', 'wrongpassword123'); // 8+ 文字 → HTML5 通過
    await page.click('button[type="submit"]');
    // 401 で同じ /login ページを返す。Playwright は通常のフォーム submit ではページ更新を待たないので、
    // ロケータの timeout で待ち、サーバから返ってきた notif を確認する。
    await expect(page.locator('.notif[data-kind="warn"]')).toBeVisible({ timeout: 10_000 });
    await expect(page).toHaveURL(/\/login/);
  });

  test('logout clears session', async ({ page, request, baseURL }) => {
    const url = baseURL ?? 'http://localhost:8080';
    await signupAndLogin(request, url);
    const cookies = await request.storageState();
    await page.context().addCookies(cookies.cookies);

    await page.goto('/');
    await expect(page).toHaveTitle(/ホーム/);

    // /settings にあるログアウトボタン (data-logout) を経由
    await page.goto('/settings');
    const csrf = (await page.context().cookies()).find(c => c.name === '__Host-sync_csrf')?.value;
    expect(csrf).toBeTruthy();

    await page.click('a[data-logout]');
    await expect(page).toHaveURL(/\/login/);
  });
});
