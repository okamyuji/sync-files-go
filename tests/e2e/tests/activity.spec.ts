import { test, expect } from '@playwright/test';
import { signupAndLogin } from '../fixtures/auth';

test.describe('activity timeline', () => {
  test('shows signup + login + upload entries', async ({ page, request, baseURL }) => {
    const url = baseURL ?? 'http://localhost:8080';
    const auth = await signupAndLogin(request, url);
    const cookies = await request.storageState();
    await page.context().addCookies(cookies.cookies);

    // generate one upload event
    await request.post(`${url}/api/files`, {
      headers: { 'X-CSRF-Token': auth.csrf, 'X-File-Path': '/activity.txt', 'If-None-Match': '*' },
      data: Buffer.from('audit me\n'),
    });

    await page.goto('/activity');
    await expect(page.locator('h1', { hasText: 'アクティビティ' })).toBeVisible();
    const items = page.locator('.timeline-item');
    await expect(items.first()).toBeVisible();
    // 直近 3 件にサインアップ・ログイン・アップロードのいずれかが入る
    const text = await page.locator('.timeline').innerText();
    expect(text).toMatch(/アカウント作成|ログイン|アップロード/);
  });
});
