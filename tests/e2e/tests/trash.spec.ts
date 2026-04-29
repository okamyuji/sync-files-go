import { test, expect } from '@playwright/test';
import { signupAndLogin } from '../fixtures/auth';

test.describe('trash flow', () => {
  test('upload → delete → trash list → restore', async ({ page, request, baseURL }) => {
    const url = baseURL ?? 'http://localhost:8080';
    const auth = await signupAndLogin(request, url);
    const cookies = await request.storageState();
    await page.context().addCookies(cookies.cookies);

    // upload via API
    const up = await request.post(`${url}/api/files`, {
      headers: { 'X-CSRF-Token': auth.csrf, 'X-File-Path': '/trashed.txt', 'If-None-Match': '*' },
      data: Buffer.from('to be trashed\n'),
    });
    expect(up.status()).toBe(201);
    const fileID = up.headers()['x-file-id'];

    // delete
    const del = await request.delete(`${url}/api/files/${fileID}`, {
      headers: { 'X-CSRF-Token': auth.csrf },
    });
    expect(del.status()).toBe(200);

    // trash page should show it
    await page.goto('/trash');
    await expect(page.locator('h1', { hasText: 'ゴミ箱' })).toBeVisible();
    await expect(page.locator('.file-name', { hasText: 'trashed.txt' })).toBeVisible();

    // restore via HTMX button
    await page.click(`#trash-${fileID} button[hx-post*="restore"]`);
    // HTMX swap removes the row
    await expect(page.locator(`#trash-${fileID}`)).toHaveCount(0);

    // home should show it again
    await page.goto('/');
    await expect(page.locator('.file-name', { hasText: 'trashed.txt' })).toBeVisible();
  });
});
