import { test, expect } from '@playwright/test';
import { signupAndLogin } from '../fixtures/auth';

test.describe('upload', () => {
  test('drop zone visible on home', async ({ page, request, baseURL }) => {
    const url = baseURL ?? 'http://localhost:8080';
    await signupAndLogin(request, url);
    const cookies = await request.storageState();
    await page.context().addCookies(cookies.cookies);

    await page.goto('/');
    await expect(page.locator('[data-upload-zone]')).toBeVisible();
    await expect(page.locator('[data-upload-form]')).toBeVisible();
    await expect(page.locator('[data-upload-form] input[type=file]')).toBeVisible();
  });

  test('upload via XHR shows file in list', async ({ page, request, baseURL }) => {
    const url = baseURL ?? 'http://localhost:8080';
    await signupAndLogin(request, url);
    const cookies = await request.storageState();
    await page.context().addCookies(cookies.cookies);

    await page.goto('/');

    // 仮想ファイルを setInputFiles で投入し、アップロードボタンを押す
    const buf = Buffer.from('hello e2e\n');
    await page.setInputFiles('[data-upload-form] input[type=file]', {
      name: 'e2e-upload.txt', mimeType: 'text/plain', buffer: buf,
    });
    await page.click('[data-upload-form] button[type="submit"]');

    // app.js の onDone が location.reload() するので、リロード後にファイル一覧に表示
    await page.waitForLoadState('networkidle');
    await expect(page.locator('table.file-table')).toBeVisible();
    await expect(page.locator('.file-name', { hasText: 'e2e-upload.txt' })).toBeVisible();
  });
});
