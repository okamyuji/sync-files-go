// _screenshots.spec.ts
//
// 記事用のスクリーンショット採集スクリプト。docs/screenshots/ に保存する。
// 実装内容を読者にビジュアルで伝える目的。
import { test } from '@playwright/test';
import { signupAndLogin } from '../fixtures/auth';
import * as path from 'path';

const OUT = path.resolve(__dirname, '../../../docs/screenshots');

test.use({
  viewport: { width: 1280, height: 800 },
  deviceScaleFactor: 2, // Retina
});

async function shoot(page: any, name: string) {
  await page.screenshot({ path: path.join(OUT, name + '.png'), fullPage: true });
}

test.describe('screenshots for article', () => {
  test('login page', async ({ page }) => {
    await page.goto('/login');
    await page.waitForLoadState('networkidle');
    await shoot(page, '01-login');
  });

  test('signup page', async ({ page }) => {
    await page.goto('/signup');
    await page.waitForLoadState('networkidle');
    await shoot(page, '02-signup');
  });

  test('home empty', async ({ page, request, baseURL }) => {
    const url = baseURL ?? 'http://localhost:8080';
    await signupAndLogin(request, url);
    await page.context().addCookies((await request.storageState()).cookies);
    await page.goto('/');
    await page.waitForLoadState('networkidle');
    await shoot(page, '03-home-empty');
  });

  test('home with files', async ({ page, request, baseURL }) => {
    const url = baseURL ?? 'http://localhost:8080';
    const auth = await signupAndLogin(request, url);
    // Upload 3 サンプルファイル
    for (const [name, body] of [
      ['report-q2.docx', 'Q2 サマリレポートのサンプル本文'],
      ['photo-2026-04-29.jpg', 'バイナリ風サンプル'],
      ['notes.txt', 'メモ書き'],
    ] as const) {
      await request.post(`${url}/api/files`, {
        headers: { 'X-CSRF-Token': auth.csrf, 'X-File-Path': '/' + name, 'If-None-Match': '*' },
        data: Buffer.from(body),
      });
    }
    await page.context().addCookies((await request.storageState()).cookies);
    await page.goto('/');
    await page.waitForLoadState('networkidle');
    await shoot(page, '04-home-with-files');
  });

  test('conflict modal (open via JS)', async ({ page, request, baseURL }) => {
    const url = baseURL ?? 'http://localhost:8080';
    const auth = await signupAndLogin(request, url);
    await request.post(`${url}/api/files`, {
      headers: { 'X-CSRF-Token': auth.csrf, 'X-File-Path': '/conflict-demo.txt', 'If-None-Match': '*' },
      data: Buffer.from('original'),
    });
    await page.context().addCookies((await request.storageState()).cookies);
    await page.goto('/');
    // モーダルを直接開く（実 conflict 経由は省略）
    await page.evaluate(() => {
      const ev = new CustomEvent('openConflictModal', { detail: {
        kind: 'version_mismatch',
        file: { id: 'demo', path: '/conflict-demo.txt', current_version_id: 'aabbccdd-1234-5678-9abc-def012345678', current_modified_at: new Date().toISOString() }
      }});
      document.body.dispatchEvent(ev);
    });
    await page.waitForTimeout(300);
    await shoot(page, '05-conflict-modal');
  });

  test('trash empty', async ({ page, request, baseURL }) => {
    const url = baseURL ?? 'http://localhost:8080';
    await signupAndLogin(request, url);
    await page.context().addCookies((await request.storageState()).cookies);
    await page.goto('/trash');
    await page.waitForLoadState('networkidle');
    await shoot(page, '06-trash-empty');
  });

  test('share-links empty', async ({ page, request, baseURL }) => {
    const url = baseURL ?? 'http://localhost:8080';
    await signupAndLogin(request, url);
    await page.context().addCookies((await request.storageState()).cookies);
    await page.goto('/share-links');
    await page.waitForLoadState('networkidle');
    await shoot(page, '07-share-links');
  });

  test('activity', async ({ page, request, baseURL }) => {
    const url = baseURL ?? 'http://localhost:8080';
    const auth = await signupAndLogin(request, url);
    await request.post(`${url}/api/files`, {
      headers: { 'X-CSRF-Token': auth.csrf, 'X-File-Path': '/audit-demo.txt', 'If-None-Match': '*' },
      data: Buffer.from('audit'),
    });
    await page.context().addCookies((await request.storageState()).cookies);
    await page.goto('/activity');
    await page.waitForLoadState('networkidle');
    await shoot(page, '08-activity');
  });

  test('settings (TOTP not yet)', async ({ page, request, baseURL }) => {
    const url = baseURL ?? 'http://localhost:8080';
    await signupAndLogin(request, url);
    await page.context().addCookies((await request.storageState()).cookies);
    await page.goto('/settings');
    await page.waitForLoadState('networkidle');
    await shoot(page, '09-settings');
  });

  test('TOTP setup with QR', async ({ page, request, baseURL }) => {
    const url = baseURL ?? 'http://localhost:8080';
    await signupAndLogin(request, url);
    await page.context().addCookies((await request.storageState()).cookies);
    await page.goto('/settings/security/totp/setup');
    await page.waitForLoadState('networkidle');
    // shouldハイドレーション
    await page.waitForTimeout(200);
    await shoot(page, '10-totp-setup-qr');
  });

  test('public share landing', async ({ page, request, baseURL }) => {
    const url = baseURL ?? 'http://localhost:8080';
    const auth = await signupAndLogin(request, url);
    const up = await request.post(`${url}/api/files`, {
      headers: { 'X-CSRF-Token': auth.csrf, 'X-File-Path': '/share-demo.txt', 'If-None-Match': '*' },
      data: Buffer.from('shared content'),
    });
    const fileID = up.headers()['x-file-id'];
    const issue = await request.post(`${url}/api/files/${fileID}/share-links`, {
      headers: { 'X-CSRF-Token': auth.csrf, 'Content-Type': 'application/json' },
      data: { expires_in: '1h' },
    });
    const issued = await issue.json();
    const ctx = await page.context().browser()!.newContext();
    const anon = await ctx.newPage();
    await anon.goto(issued.url);
    await anon.waitForLoadState('networkidle');
    await anon.screenshot({ path: path.join(OUT, '11-public-share.png'), fullPage: true });
    await ctx.close();
  });
});
