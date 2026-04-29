// critical-path.spec.ts
//
// Phase 6 リリースゲート: ファイル損失防止のクリティカルシナリオ。
// staging で実行 → green → prod に手動 apply 可。
//
// 環境変数:
//   BASE_URL          リリース先 URL
//   E2E_TEST_EMAIL    既存の E2E 専用ユーザのメール（prod では別途事前作成）
//   E2E_TEST_PASSWORD 同パスワード
//
// 注: このテストは PROD 環境で「実データ」を作る。プロジェクトポリシーに合わせ、
// staging のみで自動実行し、prod は手動オペレータが目視確認することを推奨。
import { test, expect } from '@playwright/test';

const baseURL = process.env.BASE_URL ?? 'https://sync-staging.example.com';
const email = process.env.E2E_TEST_EMAIL ?? '';
const password = process.env.E2E_TEST_PASSWORD ?? '';

test.skip(!email || !password, 'E2E_TEST_EMAIL / E2E_TEST_PASSWORD 未設定');

test.describe('release critical path', () => {
  test('OCC: stale If-Match returns 409 with full options', async ({ playwright }) => {
    const ctx = await playwright.request.newContext({
      baseURL, ignoreHTTPSErrors: !!process.env.ALLOW_INSECURE,
    });
    // CSRF cookie 取得
    await ctx.get('/login');
    const cookies = (await ctx.storageState()).cookies;
    const csrf = cookies.find(c => c.name === '__Host-sync_csrf')?.value;
    expect(csrf, 'csrf cookie set').toBeTruthy();

    // login
    const li = await ctx.post('/api/login', {
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf! },
      data: { email, password },
    });
    expect(li.status()).toBe(200);

    // upload v1
    const path = `/release-e2e-${Date.now()}.txt`;
    const u1 = await ctx.post('/api/files', {
      headers: { 'X-CSRF-Token': csrf!, 'X-File-Path': path, 'If-None-Match': '*' },
      data: Buffer.from('release v1\n'),
    });
    expect(u1.status()).toBe(201);
    const fileID = u1.headers()['x-file-id'];

    // stale If-Match
    const stale = '00000000-0000-0000-0000-000000000000';
    const conflict = await ctx.post('/api/files', {
      headers: { 'X-CSRF-Token': csrf!, 'X-File-Path': path, 'If-Match': stale },
      data: Buffer.from('release v2\n'),
    });
    expect(conflict.status()).toBe(409);
    const body = await conflict.json();
    expect(body.kind).toBe('version_mismatch');
    expect(body.options.length).toBeGreaterThanOrEqual(3);

    // cleanup: delete then DELETE the file
    await ctx.delete(`/api/files/${fileID}`, { headers: { 'X-CSRF-Token': csrf! } });
    await ctx.dispose();
  });

  test('Soft delete + restore preserves data (INV-1)', async ({ playwright }) => {
    const ctx = await playwright.request.newContext({
      baseURL, ignoreHTTPSErrors: !!process.env.ALLOW_INSECURE,
    });
    await ctx.get('/login');
    const csrf = ((await ctx.storageState()).cookies.find(c => c.name === '__Host-sync_csrf'))?.value;
    expect(csrf).toBeTruthy();
    await ctx.post('/api/login', {
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf! },
      data: { email, password },
    });

    const path = `/release-restore-${Date.now()}.txt`;
    const up = await ctx.post('/api/files', {
      headers: { 'X-CSRF-Token': csrf!, 'X-File-Path': path, 'If-None-Match': '*' },
      data: Buffer.from('keep me\n'),
    });
    const fileID = up.headers()['x-file-id'];

    // delete
    await ctx.delete(`/api/files/${fileID}`, { headers: { 'X-CSRF-Token': csrf! } });
    // restore
    const r = await ctx.post(`/api/files/${fileID}/restore`, { headers: { 'X-CSRF-Token': csrf! } });
    expect(r.status()).toBe(200);

    // download → 元の内容と一致
    const dl = await ctx.get(`/api/files/${fileID}`);
    expect(dl.status()).toBe(200);
    expect(await dl.text()).toBe('keep me\n');

    // cleanup
    await ctx.delete(`/api/files/${fileID}`, { headers: { 'X-CSRF-Token': csrf! } });
    await ctx.dispose();
  });

  test('public share link: issue → unauthenticated download', async ({ playwright, browser }) => {
    const authCtx = await playwright.request.newContext({
      baseURL, ignoreHTTPSErrors: !!process.env.ALLOW_INSECURE,
    });
    await authCtx.get('/login');
    const csrf = ((await authCtx.storageState()).cookies.find(c => c.name === '__Host-sync_csrf'))?.value;
    await authCtx.post('/api/login', {
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf! },
      data: { email, password },
    });

    const up = await authCtx.post('/api/files', {
      headers: { 'X-CSRF-Token': csrf!, 'X-File-Path': `/release-share-${Date.now()}.txt`, 'If-None-Match': '*' },
      data: Buffer.from('shared\n'),
    });
    const fileID = up.headers()['x-file-id'];

    const issue = await authCtx.post(`/api/files/${fileID}/share-links`, {
      headers: { 'X-CSRF-Token': csrf!, 'Content-Type': 'application/json' },
      data: { expires_in: '1h' },
    });
    const issued = await issue.json();
    const shareURL = issued.url as string;

    // unauthenticated context で landing + download
    const anonCtx = await browser.newContext({ ignoreHTTPSErrors: !!process.env.ALLOW_INSECURE });
    const anon = await anonCtx.newPage();
    await anon.goto(shareURL);
    await expect(anon.locator('a.btn-primary', { hasText: 'ダウンロード' })).toBeVisible();
    await anonCtx.close();

    await authCtx.delete(`/api/files/${fileID}`, { headers: { 'X-CSRF-Token': csrf! } });
    await authCtx.dispose();
  });
});
