import { APIRequestContext, expect } from '@playwright/test';

/**
 * 一意なメールアドレスを生成する。並列実行・複数 spec での衝突を避けるため、
 * 「タイムスタンプ + ランダム」を使う。
 */
export function uniqueEmail(prefix = 'e2e'): string {
  const ts = Date.now().toString(36);
  const rand = Math.random().toString(36).slice(2, 8);
  return `${prefix}-${ts}-${rand}@example.com`;
}

/**
 * APIRequestContext に対して /api/signup と /api/login を順番に呼んで認証済みコンテキストにする。
 *
 * CSRF Cookie は最初の GET /signup で発行される。`request.get()` は
 * Set-Cookie を自動で保持するので、後続の POST はそのまま動く。
 *
 * 戻り値は {email, password, csrf, recoveryCodes} と APIRequestContext そのもの。
 */
export async function signupAndLogin(
  request: APIRequestContext,
  baseURL: string,
  passwordOverride?: string,
): Promise<{ email: string; password: string; csrf: string; recoveryCodes: string[] }> {
  const email = uniqueEmail();
  const password = passwordOverride ?? 'testpass-' + Math.random().toString(36).slice(2, 8);

  // 1) CSRF Cookie を発行させる
  await request.get(`${baseURL}/signup`);
  const cookies = await request.storageState();
  const csrf = cookies.cookies.find(c => c.name === '__Host-sync_csrf')?.value;
  if (!csrf) throw new Error('CSRF cookie not set after GET /signup');

  // 2) サインアップ
  const su = await request.post(`${baseURL}/api/signup`, {
    headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
    data: { email, password },
  });
  expect(su.status(), 'signup must return 201').toBe(201);
  const suBody = await su.json();
  const recoveryCodes = (suBody.recovery_codes ?? []) as string[];

  // 3) ログイン → __Host-sync_session が乗る
  const li = await request.post(`${baseURL}/api/login`, {
    headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
    data: { email, password },
  });
  expect(li.status(), 'login must return 200').toBe(200);

  return { email, password, csrf, recoveryCodes };
}
