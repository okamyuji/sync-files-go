import { test, expect } from '@playwright/test';
import { signupAndLogin } from '../fixtures/auth';

test.describe('public share', () => {
  test('issued link landing page renders without auth, download works', async ({ page, request, baseURL }) => {
    const url = baseURL ?? 'http://localhost:8080';
    const auth = await signupAndLogin(request, url);

    // upload + issue
    const up = await request.post(`${url}/api/files`, {
      headers: { 'X-CSRF-Token': auth.csrf, 'X-File-Path': '/public.txt', 'If-None-Match': '*' },
      data: Buffer.from('public content\n'),
    });
    const fileID = up.headers()['x-file-id'];
    const issue = await request.post(`${url}/api/files/${fileID}/share-links`, {
      headers: { 'X-CSRF-Token': auth.csrf, 'Content-Type': 'application/json' },
      data: { expires_in: '1h' },
    });
    const issued = await issue.json();
    const shareURL = issued.url as string;
    expect(shareURL).toContain('/share/');

    // 未認証コンテキストで visit
    const anonCtx = await page.context().browser()?.newContext();
    if (!anonCtx) throw new Error('cannot create anon context');
    const anonPage = await anonCtx.newPage();
    await anonPage.goto(shareURL);
    await expect(anonPage).toHaveTitle(/public\.txt/);
    await expect(anonPage.locator('h1', { hasText: 'public.txt' })).toBeVisible();
    await expect(anonPage.locator('a.btn-primary', { hasText: 'ダウンロード' })).toBeVisible();
    await anonCtx.close();
  });
});
