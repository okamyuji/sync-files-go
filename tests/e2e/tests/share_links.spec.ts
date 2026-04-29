import { test, expect } from '@playwright/test';
import { signupAndLogin } from '../fixtures/auth';

test.describe('share links', () => {
  test('issue → list → revoke', async ({ page, request, baseURL }) => {
    const url = baseURL ?? 'http://localhost:8080';
    const auth = await signupAndLogin(request, url);
    const cookies = await request.storageState();
    await page.context().addCookies(cookies.cookies);

    // upload + issue
    const up = await request.post(`${url}/api/files`, {
      headers: { 'X-CSRF-Token': auth.csrf, 'X-File-Path': '/shareme.txt', 'If-None-Match': '*' },
      data: Buffer.from('share me\n'),
    });
    const fileID = up.headers()['x-file-id'];
    const issue = await request.post(`${url}/api/files/${fileID}/share-links`, {
      headers: { 'X-CSRF-Token': auth.csrf, 'Content-Type': 'application/json' },
      data: { expires_in: '1h' },
    });
    expect(issue.status()).toBe(201);

    await page.goto('/share-links');
    await expect(page.locator('h1', { hasText: '共有リンク' })).toBeVisible();
    const row = page.locator('tr[id^="share-"]').first();
    await expect(row).toBeVisible();
    await expect(row).toContainText('shareme.txt');

    // revoke via HTMX (with confirm dialog auto-accept)
    page.on('dialog', d => d.accept());
    await row.locator('button[hx-delete]').click();
    await expect(page.locator('tr[id^="share-"]')).toHaveCount(0);
  });
});
