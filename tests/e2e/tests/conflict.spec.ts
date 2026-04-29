import { test, expect } from '@playwright/test';
import { signupAndLogin } from '../fixtures/auth';

test.describe('conflict modal (OCC 409)', () => {
  test('save_as_copy button creates a conflict copy with proper name', async ({ page, request, baseURL }) => {
    const url = baseURL ?? 'http://localhost:8080';
    const auth = await signupAndLogin(request, url);
    const cookies = await request.storageState();
    await page.context().addCookies(cookies.cookies);

    // 1) 最初に正常アップロード
    const upload1 = await request.post(`${url}/api/files`, {
      headers: { 'X-CSRF-Token': auth.csrf, 'X-File-Path': '/conflict.txt', 'If-None-Match': '*' },
      data: Buffer.from('original content\n'),
    });
    expect(upload1.status()).toBe(201);
    const fileID = upload1.headers()['x-file-id'];
    expect(fileID).toBeTruthy();

    // 2) 古い If-Match で再アップロード → 409 + JSON
    const stale = '00000000-0000-0000-0000-000000000000';
    const conflict = await request.post(`${url}/api/files`, {
      headers: { 'X-CSRF-Token': auth.csrf, 'X-File-Path': '/conflict.txt', 'If-Match': stale },
      data: Buffer.from('alt content\n'),
    });
    expect(conflict.status()).toBe(409);
    const body = await conflict.json();
    expect(body.kind).toBe('version_mismatch');
    expect(body.file.id).toBe(fileID);
    expect(body.options.length).toBeGreaterThanOrEqual(3);
    const ids = body.options.map((o: any) => o.id);
    expect(ids).toContain('save_as_copy');
    expect(ids).toContain('force_overwrite');
    expect(ids).toContain('view_server');

    // 3) save_as_copy エンドポイント直接呼び出し
    const copy = await request.post(`${url}/api/files/${fileID}/save-as-copy`, {
      headers: { 'X-CSRF-Token': auth.csrf, 'X-Device-Label': 'PlaywrightTest' },
      data: Buffer.from('alt content\n'),
    });
    expect(copy.status()).toBe(201);
    const newPath = copy.headers()['x-file-path'];
    expect(newPath).toMatch(/\/conflict \(conflict \d{4}-\d{2}-\d{2} \d{2}-\d{2} device-PlaywrightTest\)\.txt/);

    // 4) ホームに 2 ファイル並ぶ
    await page.goto('/');
    await expect(page.locator('.file-name', { hasText: 'conflict.txt' })).toHaveCount(1);
    await expect(page.locator('.file-name', { hasText: 'conflict (conflict' })).toHaveCount(1);
  });

  test('force_overwrite via If-Match: * succeeds', async ({ request, baseURL }) => {
    const url = baseURL ?? 'http://localhost:8080';
    const auth = await signupAndLogin(request, url);

    await request.post(`${url}/api/files`, {
      headers: { 'X-CSRF-Token': auth.csrf, 'X-File-Path': '/force.txt', 'If-None-Match': '*' },
      data: Buffer.from('v1\n'),
    });
    const force = await request.post(`${url}/api/files`, {
      headers: { 'X-CSRF-Token': auth.csrf, 'X-File-Path': '/force.txt', 'If-Match': '*' },
      data: Buffer.from('v2-force\n'),
    });
    expect(force.status()).toBe(204);
  });
});
